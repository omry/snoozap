// Package ipc implements the fixed, local-only owner command protocol.
package ipc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/history"
	"github.com/omry/snoozap/internal/model"
)

const (
	Version  = 1
	MaxFrame = 1 << 20
	// Configuration parsing permits 128 nested JSON containers; the request
	// envelope adds one container before those arguments reach the parser.
	maxDecodeDepth = 129
	ioTimeout      = 5 * time.Second
)

type Conn interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}
type Dialer func(context.Context) (Conn, error)

// Platform adapters install only local OS-user-restricted transports.
var listenLocal func(context.Context, model.Service) (io.Closer, error)
var dialLocal Dialer

func Listen(ctx context.Context, service model.Service) (io.Closer, error) {
	if listenLocal == nil {
		return nil, model.ErrUnavailable
	}
	return listenLocal(ctx, service)
}
func Dial(ctx context.Context) (Conn, error) {
	if dialLocal == nil {
		return nil, model.ErrUnavailable
	}
	return dialLocal(ctx)
}

type request struct {
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	Operation string          `json:"operation"`
	Args      json.RawMessage `json:"args,omitempty"`
}
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type response struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(header[:])
	if n == 0 || n > MaxFrame {
		return nil, errors.New("invalid IPC frame size")
	}
	data := make([]byte, int(n))
	_, err := io.ReadFull(r, data)
	return data, err
}
func writeFrame(w io.Writer, data []byte) error {
	if len(data) == 0 || len(data) > MaxFrame {
		return errors.New("invalid IPC frame size")
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	for _, b := range [][]byte{header[:], data} {
		for len(b) > 0 {
			n, err := w.Write(b)
			if err != nil {
				return err
			}
			if n <= 0 || n > len(b) {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
	}
	return nil
}

// Reject duplicate keys and deep envelopes before typed decoding. Configuration
// is also validated by its stricter existing parser before it reaches Service.
func decode(data []byte, into any) error {
	if !utf8.Valid(data) {
		return model.ErrInvalidInput
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > maxDecodeDepth {
			return model.ErrInvalidInput
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := t.(string)
				if !ok || seen[key] {
					return model.ErrInvalidInput
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return model.ErrInvalidInput
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return model.ErrInvalidInput
	}
	if _, err := d.Token(); err != io.EOF {
		return model.ErrInvalidInput
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return model.ErrInvalidInput
	}
	allowed := map[string]bool{}
	t := reflect.TypeOf(into).Elem()
	for i := 0; i < t.NumField(); i++ {
		allowed[strings.Split(t.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for key, raw := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return model.ErrInvalidInput
		}
	}
	if err := d.Decode(into); err != nil {
		return model.ErrInvalidInput
	}
	return nil
}
func validID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}
func result(id string, v any, err *wireError) []byte {
	r := response{Version: Version, ID: id, Error: err}
	if err == nil {
		r.Result, _ = json.Marshal(v)
	}
	data, e := json.Marshal(r)
	if e != nil || len(data) > MaxFrame {
		data, _ = json.Marshal(response{Version: Version, ID: id, Error: &wireError{"result-too-large", "Result exceeds the local protocol limit"}})
	}
	return data
}

func execute(ctx context.Context, s model.Service, r request) (any, *wireError) {
	invalid := &wireError{"invalid-input", "Invalid command or configuration"}
	none := len(r.Args) == 0 || bytes.Equal(r.Args, []byte("{}"))
	var v any
	var err error
	switch r.Operation {
	case "status", "history", "history-clear", "config-export", "stop":
		if !none {
			return nil, invalid
		}
		switch r.Operation {
		case "status":
			v, err = s.Snapshot(ctx)
		case "history":
			v, err = s.History(ctx)
		case "history-clear":
			err = s.ClearHistory(ctx)
			v = struct{}{}
		case "config-export":
			var data []byte
			data, err = s.ExportConfig(ctx)
			v = json.RawMessage(data)
		case "stop":
			v, err = s.Execute(ctx, model.Command{Operation: "stop"})
		}
	case "mode":
		var a struct {
			Mode     model.Mode `json:"mode"`
			Duration string     `json:"duration,omitempty"`
		}
		if decode(r.Args, &a) != nil || !a.Mode.Valid() {
			return nil, invalid
		}
		var duration time.Duration
		if a.Duration != "" {
			duration, err = time.ParseDuration(a.Duration)
			if err != nil || duration <= 0 || a.Mode == model.Auto {
				return nil, invalid
			}
		}
		v, err = s.Execute(ctx, model.Command{Operation: "mode", Mode: a.Mode, Duration: duration})
	case "monitor":
		var a struct {
			Enabled *bool `json:"enabled"`
		}
		if decode(r.Args, &a) != nil || a.Enabled == nil {
			return nil, invalid
		}
		v, err = s.Execute(ctx, model.Command{Operation: "monitor", Monitoring: *a.Enabled})
	case "config":
		cfg, e := config.Parse(bytes.NewReader(r.Args))
		if e != nil {
			return nil, invalid
		}
		v, err = s.Execute(ctx, model.Command{Operation: "config", Configuration: &cfg})
	default:
		return nil, &wireError{"unavailable", "Unknown or unavailable local operation"}
	}
	if err != nil {
		return nil, &wireError{"operation-failed", "Owner operation failed; inspect current status before retrying"}
	}
	return v, nil
}

// ServeConn never owns the engine lifetime. Disconnecting a caller only closes
// this connection. All deadlines cover an entire frame, not each partial read.
func ServeConn(ctx context.Context, conn Conn, s model.Service) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	type saved struct {
		digest [32]byte
		data   []byte
	}
	cache := map[string]saved{}
	cachedBytes := 0
	for {
		if conn.SetReadDeadline(time.Now().Add(ioTimeout)) != nil {
			return
		}
		data, err := readFrame(conn)
		if err != nil {
			return
		}
		var r request
		if decode(data, &r) != nil || !validID(r.ID) {
			return
		}
		var reply []byte
		if r.Version != Version {
			reply = result(r.ID, nil, &wireError{"version-mismatch", "Incompatible local protocol; upgrade SnooZap clients and owner together"})
		} else if r.Operation == "subscribe" {
			if len(r.Args) > 0 && !bytes.Equal(r.Args, []byte("{}")) {
				reply = result(r.ID, nil, &wireError{"invalid-input", "Subscription takes no arguments"})
			} else {
				serveSubscription(ctx, conn, s, r.ID)
				return
			}
		} else {
			mutation := r.Operation == "mode" || r.Operation == "monitor" || r.Operation == "config" || r.Operation == "history-clear" || r.Operation == "stop"
			digest := sha256.Sum256(data)
			if prior, exists := cache[r.ID]; exists {
				if prior.digest != digest {
					reply = result(r.ID, nil, &wireError{"id-conflict", "Request ID was already used for another mutation"})
				} else {
					reply = prior.data
				}
			} else if mutation && (len(cache) >= 32 || cachedBytes >= MaxFrame) {
				reply = result(r.ID, nil, &wireError{"connection-limit", "Connection mutation limit reached; inspect status before opening a new connection"})
			} else {
				callCtx, done := context.WithTimeout(ctx, ioTimeout)
				v, e := execute(callCtx, s, r)
				done()
				reply = result(r.ID, v, e)
				if mutation {
					cache[r.ID] = saved{digest, reply}
					cachedBytes += len(reply)
				}
			}
		}
		if conn.SetWriteDeadline(time.Now().Add(ioTimeout)) != nil || writeFrame(conn, reply) != nil {
			return
		}
		if r.Version != Version || r.Operation == "stop" {
			return
		}
	}
}

func serveSubscription(ctx context.Context, conn Conn, s model.Service, id string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates, err := s.Subscribe(ctx)
	if err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(ioTimeout))
		_ = writeFrame(conn, result(id, nil, &wireError{"operation-failed", "Status subscription unavailable"}))
		return
	}
	if conn.SetReadDeadline(time.Time{}) != nil {
		return
	}
	// EOF, cancellation or any extra client input ends the stream. This reader
	// prevents an idle disconnected view from retaining an engine subscription.
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			if conn.SetWriteDeadline(time.Now().Add(ioTimeout)) != nil || writeFrame(conn, result(id, snapshot, nil)) != nil {
				return
			}
		}
	}
}

// Client opens one connection per operation. It never retries a mutation.
// Close cancels its own streams; only an explicit stop command stops the owner.
type Client struct {
	dial     Dialer
	ctx      context.Context
	cancel   context.CancelFunc
	sequence atomic.Uint64
}

func NewClient(dial Dialer) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{dial: dial, ctx: ctx, cancel: cancel}
}
func (c *Client) Close(context.Context) error { c.cancel(); return nil }
func (c *Client) open(ctx context.Context, operation string, args any) (Conn, string, func(), error) {
	if ctx.Err() != nil {
		return nil, "", nil, ctx.Err()
	}
	if c.ctx.Err() != nil {
		return nil, "", nil, model.ErrStopped
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	stopOwner := context.AfterFunc(c.ctx, func() { _ = conn.Close() })
	stopCall := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cleanup := func() { stopCall(); stopOwner(); _ = conn.Close() }
	id := strconv.FormatUint(c.sequence.Add(1), 10)
	r := request{Version: Version, ID: id, Operation: operation}
	if args != nil {
		r.Args, err = json.Marshal(args)
	}
	data, e := json.Marshal(r)
	if err == nil {
		err = e
	}
	deadline := time.Now().Add(ioTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err == nil {
		err = conn.SetWriteDeadline(deadline)
	}
	if err == nil {
		err = conn.SetReadDeadline(deadline)
	}
	if err == nil {
		err = writeFrame(conn, data)
	}
	if err != nil {
		cleanup()
		return nil, "", nil, errors.New("Local request failed; inspect current status before retrying")
	}
	return conn, id, cleanup, nil
}
func readResponse(conn Conn, id string, into any) error {
	data, err := readFrame(conn)
	if err != nil {
		return errors.New("Local result unavailable; inspect current status before retrying")
	}
	var r response
	// Response readers permit additional version-1 fields.
	if !utf8.Valid(data) || json.Unmarshal(data, &r) != nil || r.Version != Version || r.ID != id || (r.Error == nil) == (len(r.Result) == 0) {
		return errors.New("Invalid owner response; upgrade SnooZap if versions differ")
	}
	if r.Error != nil {
		switch r.Error.Code {
		case "invalid-input":
			return model.ErrInvalidInput
		case "unavailable":
			return model.ErrUnavailable
		case "version-mismatch":
			return errors.New("Incompatible local protocol; upgrade SnooZap clients and owner together")
		default:
			return errors.New("Owner operation failed; inspect current status before retrying")
		}
	}
	if json.Unmarshal(r.Result, into) != nil {
		return errors.New("Invalid owner result")
	}
	return nil
}
func (c *Client) call(ctx context.Context, operation string, args, into any) error {
	conn, id, cleanup, err := c.open(ctx, operation, args)
	if err != nil {
		return err
	}
	defer cleanup()
	return readResponse(conn, id, into)
}
func (c *Client) Snapshot(ctx context.Context) (model.Snapshot, error) {
	var s model.Snapshot
	err := c.call(ctx, "status", nil, &s)
	if err == nil && !validSnapshot(s) {
		err = errors.New("Invalid owner snapshot")
	}
	return s, err
}
func validSnapshot(s model.Snapshot) bool { return s.SchemaVersion == 1 && s.Mode.Valid() }
func (c *Client) History(ctx context.Context) (history.Snapshot, error) {
	var h history.Snapshot
	err := c.call(ctx, "history", nil, &h)
	if err == nil && h.SchemaVersion != 1 {
		err = errors.New("Invalid owner history")
	}
	return h, err
}
func (c *Client) ClearHistory(ctx context.Context) error {
	var v struct{}
	return c.call(ctx, "history-clear", nil, &v)
}
func (c *Client) ExportConfig(ctx context.Context) ([]byte, error) {
	var data json.RawMessage
	err := c.call(ctx, "config-export", nil, &data)
	return data, err
}
func (c *Client) Execute(ctx context.Context, cmd model.Command) (model.Snapshot, error) {
	var s model.Snapshot
	var args any
	switch cmd.Operation {
	case "status", "stop":
	case "mode":
		if !cmd.Mode.Valid() || cmd.Duration < 0 || cmd.Mode == model.Auto && cmd.Duration != 0 {
			return s, model.ErrInvalidInput
		}
		a := map[string]any{"mode": cmd.Mode}
		if cmd.Duration > 0 {
			a["duration"] = cmd.Duration.String()
		}
		args = a
	case "monitor":
		args = map[string]bool{"enabled": cmd.Monitoring}
	case "config":
		if cmd.Configuration == nil || config.Validate(*cmd.Configuration) != nil {
			return s, model.ErrInvalidInput
		}
		args = cmd.Configuration
	default:
		return s, model.ErrUnavailable
	}
	err := c.call(ctx, cmd.Operation, args, &s)
	if err == nil && !validSnapshot(s) {
		err = errors.New("Invalid owner snapshot")
	}
	return s, err
}
func (c *Client) Subscribe(ctx context.Context) (<-chan model.Snapshot, error) {
	conn, id, cleanup, err := c.open(ctx, "subscribe", nil)
	if err != nil {
		return nil, err
	}
	var first model.Snapshot
	if err = readResponse(conn, id, &first); err != nil || !validSnapshot(first) {
		cleanup()
		if err == nil {
			err = errors.New("Invalid owner snapshot")
		}
		return nil, err
	}
	if err = conn.SetReadDeadline(time.Time{}); err != nil {
		cleanup()
		return nil, err
	}
	updates := make(chan model.Snapshot, 1)
	updates <- first
	go func() {
		defer close(updates)
		defer cleanup()
		for {
			var s model.Snapshot
			if readResponse(conn, id, &s) != nil || !validSnapshot(s) {
				return
			}
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- s:
			case <-ctx.Done():
				return
			case <-c.ctx.Done():
				return
			}
		}
	}()
	return updates, nil
}

var _ model.Service = (*Client)(nil)
