package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/model"
)

type testPower struct{}

func (testPower) SetRequired(context.Context, bool) error { return nil }
func (testPower) Close() error                            { return nil }
func testEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e := engine.New(context.Background(), testPower{}, nil, "", engine.Options{Configuration: config.Empty(), ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }, Save: func(config.Config) error { return nil }})
	t.Cleanup(func() { e.Close(context.Background()) })
	return e
}
func boundaryConfig(t *testing.T, operator string, groups int) []byte {
	t.Helper()
	expression := config.Expression{Probe: "ac"}
	for range groups {
		group := []config.Expression{expression}
		switch operator {
		case "all":
			expression = config.Expression{All: group}
		case "any":
			expression = config.Expression{Any: group}
		default:
			t.Fatalf("unsupported expression operator %q", operator)
		}
	}
	cfg := config.Empty()
	cfg.Probes["ac"] = config.Probe{Type: "power", Enabled: true, OnStartupUnknown: "wait", OnError: "retain", GraceSeconds: 60, IntervalMS: 5000, TimeoutMS: 2000, Source: "ac"}
	cfg.Rules = []config.Rule{{ID: "boundary", Enabled: true, When: expression}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func boundaryRequest(t *testing.T, id string, args []byte) []byte {
	t.Helper()
	data, err := json.Marshal(request{Version: Version, ID: id, Operation: "config", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func testDial(t *testing.T, s model.Service) Dialer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return func(context.Context) (Conn, error) {
		a, b := net.Pipe()
		go ServeConn(ctx, b, s)
		return a, nil
	}
}
func rawConn(t *testing.T, s model.Service) Conn {
	t.Helper()
	conn, err := testDial(t, s)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return conn
}
func exchange(t *testing.T, c Conn, data string) response {
	t.Helper()
	if err := writeFrame(c, []byte(data)); err != nil {
		t.Fatal(err)
	}
	b, err := readFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	var r response
	if json.Unmarshal(b, &r) != nil {
		t.Fatal(string(b))
	}
	return r
}
func TestClientSharesOwnerStateAndDetachDoesNotStopIt(t *testing.T) {
	e := testEngine(t)
	c := NewClient(testDial(t, e))
	defer c.Close(context.Background())
	s, err := c.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Hour})
	if err != nil || s.Mode != model.Awake || !s.Hold.OSAccepted || s.Deadline == nil {
		t.Fatal(s, err)
	}
	if s, err := c.Execute(context.Background(), model.Command{Operation: "monitor", Monitoring: false}); err != nil || s.Monitoring {
		t.Fatal(s, err)
	}
	cfg := config.Empty()
	cfg.Settings.Monitoring = false
	if _, err := c.Execute(context.Background(), model.Command{Operation: "config", Configuration: &cfg}); err != nil {
		t.Fatal(err)
	}
	data, err := c.ExportConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if h, err := c.History(context.Background()); err != nil || h.SchemaVersion != 1 || len(h.Records) == 0 {
		t.Fatal(h, err)
	}
	if err := c.ClearHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h, err := c.History(context.Background()); err != nil || len(h.Records) != 0 {
		t.Fatal(h, err)
	}
	c.Close(context.Background())
	if s, err := e.Snapshot(context.Background()); err != nil || s.Mode != model.Awake || !s.Hold.OSAccepted {
		t.Fatal("client detach stopped owner", s, err)
	}
	other := NewClient(testDial(t, e))
	defer other.Close(context.Background())
	if _, err := other.Execute(context.Background(), model.Command{Operation: "stop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Snapshot(context.Background()); !errors.Is(err, model.ErrStopped) {
		t.Fatal("explicit stop did not stop owner", err)
	}
}

type countedService struct {
	model.Service
	clears atomic.Int32
}

func (s *countedService) ClearHistory(ctx context.Context) error {
	s.clears.Add(1)
	return s.Service.ClearHistory(ctx)
}
func TestConnectionDeduplicatesMutationsAndRefusesConflictingIDs(t *testing.T) {
	s := &countedService{Service: testEngine(t)}
	c := rawConn(t, s)
	request := `{"version":1,"id":"same","operation":"history-clear"}`
	a := exchange(t, c, request)
	b := exchange(t, c, request)
	if a.Error != nil || b.Error != nil || s.clears.Load() != 1 || !bytes.Equal(a.Result, b.Result) {
		t.Fatal(a, b, s.clears.Load())
	}
	r := exchange(t, c, `{"version":1,"id":"same","operation":"mode","args":{"mode":"awake"}}`)
	if r.Error == nil || r.Error.Code != "id-conflict" {
		t.Fatal(r)
	}
	state, _ := s.Snapshot(context.Background())
	if state.Mode != model.Auto {
		t.Fatal("conflicting ID executed")
	}
	for i := 0; i < 31; i++ {
		r := exchange(t, c, `{"version":1,"id":"`+time.Duration(i).String()+`","operation":"history-clear"}`)
		if r.Error != nil {
			t.Fatal(r)
		}
	}
	r = exchange(t, c, `{"version":1,"id":"overflow","operation":"history-clear"}`)
	if r.Error == nil || r.Error.Code != "connection-limit" || s.clears.Load() != 32 {
		t.Fatal("dedup cache silently evicted", r, s.clears.Load())
	}
	if r := exchange(t, c, request); r.Error != nil || s.clears.Load() != 32 {
		t.Fatal("earliest mutation was forgotten")
	}
}
func TestVersionAndInvalidOperationsCannotMutateOwner(t *testing.T) {
	e := testEngine(t)
	for _, test := range []struct{ request, code string }{
		{`{"version":2,"id":"x","operation":"mode","args":{"mode":"awake"}}`, "version-mismatch"},
		{`{"version":1,"id":"x","operation":"shell","args":{"command":"anything"}}`, "unavailable"},
		{`{"version":1,"id":"x","operation":"mode","args":{"mode":"auto","duration":"1h"}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"mode","args":{"mode":"awake","duration":"-1s"}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"monitor","args":{}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"monitor","args":{"enabled":null}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"mode","args":{"Mode":"awake"}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"mode","args":{"mode":"awake","extra":true}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"config","args":{"schema_version":2}}`, "invalid-input"},
		{`{"version":1,"id":"x","operation":"status","args":{"extra":true}}`, "invalid-input"},
	} {
		t.Run(test.code+test.request, func(t *testing.T) {
			r := exchange(t, rawConn(t, e), test.request)
			if r.Error == nil || r.Error.Code != test.code {
				t.Fatal(r)
			}
		})
	}
	s, _ := e.Snapshot(context.Background())
	if s.Mode != model.Auto || s.Hold.Requested {
		t.Fatal("rejected request mutated owner", s)
	}
}
func TestIndependentConfigBoundaryEquivalentPaths(t *testing.T) {
	for _, operator := range []string{"all", "any"} {
		t.Run(operator, func(t *testing.T) {
			data := boundaryConfig(t, operator, 31)
			cfg, err := config.Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("config.Parse rejected approved depth-32 %s expression: %v", operator, err)
			}

			direct := testEngine(t)
			directSnapshot, err := direct.Execute(context.Background(), model.Command{Operation: "config", Configuration: &cfg})
			if err != nil || directSnapshot.ConfigurationError != "" || len(directSnapshot.Rules) != 1 || directSnapshot.Rules[0].ID != "boundary" {
				t.Fatalf("direct service rejected approved depth-32 %s expression: snapshot=%+v err=%v", operator, directSnapshot, err)
			}

			attached := testEngine(t)
			reply := exchange(t, rawConn(t, attached), string(boundaryRequest(t, "boundary-"+operator, data)))
			if reply.Error != nil {
				t.Fatalf("IPC rejected approved depth-32 %s expression: %+v", operator, reply.Error)
			}
			var attachedSnapshot model.Snapshot
			if err := json.Unmarshal(reply.Result, &attachedSnapshot); err != nil {
				t.Fatal(err)
			}
			if attachedSnapshot.ConfigurationError != "" || len(attachedSnapshot.Rules) != 1 || attachedSnapshot.Rules[0].ID != "boundary" {
				t.Fatalf("IPC accepted %s expression with an invalid snapshot: %+v", operator, attachedSnapshot)
			}
			if directSnapshot.SchemaVersion != attachedSnapshot.SchemaVersion || directSnapshot.Rules[0].ID != attachedSnapshot.Rules[0].ID {
				t.Fatalf("direct and IPC paths disagree for %s expression: direct=%+v attached=%+v", operator, directSnapshot, attachedSnapshot)
			}
		})
	}
}
func TestConfigBoundaryRejectsMalformedAndOverDepthWithoutMutation(t *testing.T) {
	valid := boundaryConfig(t, "all", 31)
	malformed := append([]byte(nil), valid[:len(valid)-1]...)
	overDepth := boundaryConfig(t, "all", 32)
	if _, err := config.Parse(bytes.NewReader(malformed)); err == nil {
		t.Fatal("malformed configuration was accepted by config.Parse")
	}
	if _, err := config.Parse(bytes.NewReader(overDepth)); err == nil {
		t.Fatal("depth-33 configuration was accepted by config.Parse")
	}

	t.Run("malformed", func(t *testing.T) {
		e := testEngine(t)
		c := rawConn(t, e)
		malformedRequest := append([]byte(`{"version":1,"id":"malformed","operation":"config","args":`), malformed...)
		if err := writeFrame(c, malformedRequest); err != nil {
			t.Fatal(err)
		}
		if _, err := readFrame(c); err == nil {
			t.Fatal("malformed configuration got an IPC response")
		}
		if snapshot, err := e.Snapshot(context.Background()); err != nil || snapshot.Mode != model.Auto || len(snapshot.Rules) != 0 {
			t.Fatalf("malformed configuration mutated the owner: snapshot=%+v err=%v", snapshot, err)
		}
	})
	t.Run("over-depth", func(t *testing.T) {
		e := testEngine(t)
		reply := exchange(t, rawConn(t, e), string(boundaryRequest(t, "over-depth", overDepth)))
		if reply.Error == nil || reply.Error.Code != "invalid-input" {
			t.Fatalf("depth-33 configuration returned unexpected IPC response: %+v", reply)
		}
		if snapshot, err := e.Snapshot(context.Background()); err != nil || snapshot.Mode != model.Auto || len(snapshot.Rules) != 0 {
			t.Fatalf("depth-33 configuration mutated the owner: snapshot=%+v err=%v", snapshot, err)
		}
	})
}
func TestMalformedAndOversizedFramesCloseWithoutDispatch(t *testing.T) {
	e := testEngine(t)
	for _, data := range [][]byte{
		[]byte(`{"version":1,"id":"x","operation":"mode","operation":"stop"}`),
		[]byte(`{"version":1,"ID":"x","operation":"stop"}`),
		[]byte(`{"version":1,"id":"x","operation":"stop","unknown":true}`),
		[]byte(`{"version":1,"id":"x","operation":"mode","args":{"mode":"auto","mode":"awake"}}`),
		[]byte(`{"version":1,"id":"x","operation":"stop"} {}`),
		[]byte("{\"version\":1,\"id\":\"\xff\",\"operation\":\"stop\"}"),
		[]byte(`null`), []byte(`[]`), []byte(`{"version":1,"id":"","operation":"stop"}`),
	} {
		c := rawConn(t, e)
		go func() { _ = writeFrame(c, data) }()
		if b, err := readFrame(c); err == nil {
			t.Fatal("malformed request got response", string(b))
		}
	}
	for _, n := range []uint32{0, MaxFrame + 1, ^uint32(0)} {
		c := rawConn(t, e)
		var header [4]byte
		binary.LittleEndian.PutUint32(header[:], n)
		if _, err := c.Write(header[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := readFrame(c); err == nil {
			t.Fatal("oversized frame accepted")
		}
	}
	if s, err := e.Snapshot(context.Background()); err != nil || s.Mode != model.Auto {
		t.Fatal(s, err)
	}
}
func TestFrameWritesHandlePartialWritersAndRejectNoProgress(t *testing.T) {
	w := &shortWriter{limit: 2}
	if err := writeFrame(w, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := readFrame(bytes.NewReader(w.Bytes()))
	if err != nil || string(data) != "hello" {
		t.Fatal(string(data), err)
	}
	if err := writeFrame(&shortWriter{}, []byte("hello")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if err := writeFrame(io.Discard, make([]byte, MaxFrame+1)); err == nil {
		t.Fatal("oversized outbound frame")
	}
}

type shortWriter struct {
	bytes.Buffer
	limit int
}

func (w *shortWriter) Write(b []byte) (int, error) {
	if len(b) > w.limit {
		b = b[:w.limit]
	}
	return w.Buffer.Write(b)
}

func TestSubscriptionStreamsInitialAndLatestWithoutBlockingCommands(t *testing.T) {
	e := testEngine(t)
	client := NewClient(testDial(t, e))
	defer client.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := client.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := <-updates; s.Mode != model.Auto {
		t.Fatal(s)
	}
	for i := range 80 {
		mode := model.Awake
		if i%2 == 0 {
			mode = model.AllowSleep
		}
		if _, err := client.Execute(ctx, model.Command{Operation: "mode", Mode: mode}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-updates:
			if s.Mode == model.Awake {
				goto received
			}
		case <-deadline:
			t.Fatal("latest update not delivered")
		}
	}
received:
	cancel()
	for range updates {
	}
	if s, err := e.Snapshot(context.Background()); err != nil || !s.Hold.OSAccepted {
		t.Fatal("detaching view stopped owner", s, err)
	}
}
func TestStalledOrDisconnectedClientCannotBlockOwnerOrShutdown(t *testing.T) {
	e := testEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { ServeConn(ctx, b, e); close(done) }()
	// A partial frame must not occupy the owner goroutine.
	if _, err := a.Write([]byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	callCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := e.Execute(callCtx, model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	cancel()
	a.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled pipe retained handler")
	}
	client := NewClient(testDial(t, e))
	updates, err := client.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-updates
	client.Close(context.Background())
	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("client close did not detach stream")
		}
	case <-time.After(time.Second):
		t.Fatal("stream retained after client close")
	}
	if _, err := e.Snapshot(callCtx); err != nil {
		t.Fatal("disconnect stopped owner", err)
	}
}
func TestClientNeverRetriesAndIgnoresUntrustedErrorText(t *testing.T) {
	var calls atomic.Int32
	dial := func(context.Context) (Conn, error) {
		calls.Add(1)
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			data, _ := readFrame(b)
			var r request
			_ = json.Unmarshal(data, &r)
			_ = writeFrame(b, []byte(`{"version":1,"id":"`+r.ID+`","error":{"code":"operation-failed","message":"token=secret\\u001b[2J"}}`))
		}()
		return a, nil
	}
	c := NewClient(dial)
	defer c.Close(context.Background())
	_, err := c.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake})
	if err == nil || strings.Contains(err.Error(), "secret") || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
	for _, reply := range []string{`{"version":9,"id":"1","result":{}}`, `{"version":1,"id":"wrong","result":{}}`, `{"version":1,"id":"1","result":{},"error":{"code":"invalid-input"}}`, `{"version":1,"id":"1","result":null}`, `{"version":1,"id":"1","result":{}}`, `{"version":1,"id":"1","result":{"schema_version":2,"mode":"auto"}}`} {
		c := NewClient(func(context.Context) (Conn, error) {
			a, b := net.Pipe()
			go func() { defer b.Close(); _, _ = readFrame(b); _ = writeFrame(b, []byte(reply)) }()
			return a, nil
		})
		if _, err := c.Snapshot(context.Background()); err == nil {
			t.Fatal("invalid response accepted", reply)
		}
		c.Close(context.Background())
	}
}
func TestClientCancellationStopsBlockedReadWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	c := NewClient(func(context.Context) (Conn, error) {
		calls.Add(1)
		a, b := net.Pipe()
		go func() { defer b.Close(); _, _ = readFrame(b); var buf [1]byte; _, _ = b.Read(buf[:]) }()
		return a, nil
	})
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Snapshot(ctx); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	if time.Since(start) > time.Second || calls.Load() != 1 {
		t.Fatal("cancellation retried or hung")
	}
}

func TestIncompleteFrameDeadlineClosesClientWithoutDispatch(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	c := rawConn(t, e)
	if err := c.SetReadDeadline(time.Now().Add(ioTimeout + 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], 100)
	if _, err := c.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{'{'}); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(c); !errors.Is(err, io.EOF) {
		t.Fatal("server did not expire the incomplete frame", err)
	}
	if s, err := e.Snapshot(context.Background()); err != nil || s.Mode != model.Auto {
		t.Fatal("incomplete frame affected owner", s, err)
	}
}

func TestSlowSubscriptionWriteDeadlineLeavesOwnerResponsive(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	a, b := net.Pipe()
	defer a.Close()
	done := make(chan struct{})
	go func() { ServeConn(context.Background(), b, e); close(done) }()
	if err := writeFrame(a, []byte(`{"version":1,"id":"slow","operation":"subscribe"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.Execute(ctx, model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal("view blocked control", err)
	}
	select {
	case <-done:
	case <-time.After(ioTimeout + 2*time.Second):
		t.Fatal("slow view retained handler beyond write deadline")
	}
	if s, err := e.Snapshot(context.Background()); err != nil || !s.Hold.OSAccepted {
		t.Fatal("slow view stopped owner", s, err)
	}
}

func TestNativeWindowsPipeAttachmentAndLifetime(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("requires native Windows acceptance with no existing product owner")
	}
	e := testEngine(t)
	server, err := Listen(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(context.Background()); server.Close() })
	if other, err := Listen(context.Background(), e); err == nil {
		other.Close()
		t.Fatal("another server claimed the same user endpoint")
	}
	c := NewClient(Dial)
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s, err := c.Execute(ctx, model.Command{Operation: "mode", Mode: model.Awake}); err != nil || !s.Hold.OSAccepted {
		t.Fatal(s, err)
	}
	updates, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := <-updates; s.Mode != model.Awake {
		t.Fatal(s)
	}
	if _, err := c.Execute(ctx, model.Command{Operation: "mode", Mode: model.AllowSleep}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-updates:
		if s.Mode != model.AllowSleep {
			t.Fatal(s)
		}
	case <-ctx.Done():
		t.Fatal("native subscription missing")
	}
	if _, err := c.Execute(ctx, model.Command{Operation: "stop"}); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx); !errors.Is(err, model.ErrNoInstance) {
		t.Fatal("closed endpoint still attached", err)
	}
}

func TestNativeWindowsPipeExpiresIncompleteFrame(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("requires native Windows deadline acceptance")
	}
	e := testEngine(t)
	server, err := Listen(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(context.Background()); server.Close() })
	c, err := Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(ioTimeout + 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], 100)
	if _, err := c.Write(append(header[:], '{')); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := c.Read(one[:]); !errors.Is(err, io.EOF) {
		t.Fatal("native pipe did not expire incomplete frame", err)
	}
	client := NewClient(Dial)
	defer client.Close(context.Background())
	if s, err := client.Snapshot(context.Background()); err != nil || s.Mode != model.Auto {
		t.Fatal("stalled native client damaged endpoint", s, err)
	}
}

func TestNativeQueuedDisconnectDoesNotStopOwner(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("requires native Windows queued-client regression")
	}
	e := testEngine(t)
	server, err := Listen(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(context.Background()); server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var clients []Conn
	t.Cleanup(func() {
		for _, c := range clients {
			c.Close()
		}
	})
	for range 15 {
		c, err := Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		if err := writeFrame(c, []byte(`{"version":1,"id":"view","operation":"subscribe"}`)); err != nil {
			t.Fatal(err)
		}
		var initial model.Snapshot
		if err := readResponse(c, "view", &initial); err != nil {
			t.Fatal(err)
		}
	}
	// This client can connect to the pending native instance while all handler
	// slots are occupied, then disappear before ConnectNamedPipe is called.
	queued, err := Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued.Close()
	clients[0].Close()
	client := NewClient(Dial)
	defer client.Close(context.Background())
	if s, err := client.Snapshot(ctx); err != nil || s.Mode != model.Auto {
		t.Fatal("queued client disconnect stopped or damaged owner", s, err)
	}
	if _, err := e.Snapshot(ctx); err != nil {
		t.Fatal("disconnect stopped engine", err)
	}
}

func TestNativeRapidDisconnectPreservesProtectedPipeNamespace(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("requires native Windows namespace continuity regression")
	}
	e := testEngine(t)
	server, err := Listen(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(context.Background()); server.Close() })
	script := `$ErrorActionPreference='Stop'
Add-Type -TypeDefinition @'
using System;
using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Threading.Tasks;
public static class SnooZapNamespaceContinuity {
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CreateNamedPipe(string name,uint mode,uint pipeMode,uint max,uint output,uint input,uint timeout,IntPtr attributes);
 [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
 public static string Check() {
  string name="SnooZap.Owner."+WindowsIdentity.GetCurrent().User.Value;
  var churn=Task.Run(() => {
   for(int i=0;i<500;i++) {
    using(var p=new NamedPipeClientStream(".",name,PipeDirection.InOut,PipeOptions.Asynchronous,TokenImpersonationLevel.Identification)) {p.Connect(2000);}
   }
  });
  int attempts=0;
  do {
   IntPtr h=CreateNamedPipe(@"\\.\pipe\"+name,0x40080003,8,16,65536,65536,5000,IntPtr.Zero);
   int error=Marshal.GetLastWin32Error();
   if(h!=new IntPtr(-1)) {CloseHandle(h);throw new Exception("Protected pipe name became unowned during handoff");}
   if(error!=5) throw new Exception("First-instance rejection was not access denied: "+error);
   attempts++;
  } while(!churn.IsCompleted && attempts<100000);
  if(!churn.Wait(5000))throw new Exception("Bounded disconnect churn timed out");
  return "PASS protected namespace retained; first-instance rejections="+attempts;
 }
}
'@
[SnooZapNamespaceContinuity]::Check()`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil || !bytes.Contains(data, []byte("PASS protected namespace retained")) {
		t.Fatal(err, string(data))
	}
	client := NewClient(Dial)
	defer client.Close(context.Background())
	if _, err := client.Snapshot(ctx); err != nil {
		t.Fatal("rapid disconnects stopped owner", err)
	}
}

func nativeCLI(t *testing.T, exe string, args ...string) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("CLI invocation timed out", args)
	}
	if err == nil {
		return data, 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return data, exit.ExitCode()
}
func TestNativeAttachedCLIControlsActualOwnerAndStopsForeground(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("requires native Windows product acceptance")
	}
	exe := os.Getenv("SNOOZAP_CONSOLE_EXE")
	if exe == "" {
		t.Fatal("missing built console for native acceptance")
	}
	if data, code := nativeCLI(t, exe, "status", "--json"); code != 3 {
		t.Fatal("owner-free status implicitly started or misreported", code, string(data))
	}
	if _, code := nativeCLI(t, exe, "mode", "awake", "--for", "0s"); code != 2 {
		t.Fatal(code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := exec.CommandContext(ctx, exe, "run", "--data-dir", filepath.Join(t.TempDir(), "owner"))
	in, err := owner.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	owner.Stderr = &stderr
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		in.Close()
		if !waited {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	scanner := bufio.NewScanner(out)
	if !scanner.Scan() || !strings.Contains(scanner.Text(), "SnooZap manual console") {
		t.Fatal("owner failed startup", stderr.String())
	}
	data, code := nativeCLI(t, exe, "mode", "awake", "--for", "1h")
	if code != 0 {
		t.Fatal(code, string(data))
	}
	data, code = nativeCLI(t, exe, "status", "--json")
	var s model.Snapshot
	if code != 0 || json.Unmarshal(data, &s) != nil || s.Mode != model.Awake || !s.Hold.Requested || !s.Hold.OSAccepted || s.Deadline == nil {
		t.Fatal(code, string(data))
	}
	if len(s.Limitations) == 0 {
		t.Fatal("native limitations missing")
	}
	client := NewClient(Dial)
	defer client.Close(context.Background())
	viewCtx, detach := context.WithCancel(ctx)
	updates, err := client.Subscribe(viewCtx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := <-updates; snapshot.Mode != model.Awake {
		t.Fatal(snapshot)
	}
	if data, code := nativeCLI(t, exe, "mode", "allow-sleep"); code != 0 {
		t.Fatal(code, string(data))
	}
	select {
	case snapshot := <-updates:
		if snapshot.Mode != model.AllowSleep || snapshot.Hold.Requested {
			t.Fatal(snapshot)
		}
	case <-ctx.Done():
		t.Fatal("attached view missed native mode change")
	}
	detach()
	for range updates {
	}
	if _, code := nativeCLI(t, exe, "monitor", "off"); code != 0 {
		t.Fatal(code)
	}
	export := filepath.Join(t.TempDir(), "export with spaces.json")
	if data, code := nativeCLI(t, exe, "config", "export", export); code != 0 {
		t.Fatal(code, string(data))
	}
	data, err = os.ReadFile(export)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if data, code := nativeCLI(t, exe, "config", "apply", export); code != 0 {
		t.Fatal(code, string(data))
	}
	if data, code := nativeCLI(t, exe, "history", "--json"); code != 0 || !bytes.Contains(data, []byte(`"schema_version":1`)) {
		t.Fatal(code, string(data))
	}
	if _, code := nativeCLI(t, exe, "history", "clear"); code != 0 {
		t.Fatal(code)
	}
	if data, code := nativeCLI(t, exe, "run", "--data-dir", t.TempDir()); code != 1 || !strings.Contains(string(data), "already has an owner") {
		t.Fatal("data-dir bypassed singleton", code, string(data))
	}
	if data, code := nativeCLI(t, exe, "stop"); code != 0 {
		t.Fatal("remote stop ACK failed", code, string(data))
	}
	if err := owner.Wait(); err != nil {
		t.Fatal("foreground retained stopped owner", err, stderr.String())
	}
	waited = true
	if data, code := nativeCLI(t, exe, "status"); code != 3 {
		t.Fatal("stopped owner still accepts commands", code, string(data))
	}
}

func TestNativePipeRejectsAnonymousAndRemoteClients(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("requires native Windows pipe access checks")
	}
	e := testEngine(t)
	server, err := Listen(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(context.Background()); server.Close() })
	// Test another security principal without creating accounts or elevating.
	// The impersonation exists only inside this disposable PowerShell process.
	script := `$ErrorActionPreference='Stop'
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Security.Principal;
public static class SnooZapPipeAccess {
 [DllImport("kernel32.dll")] static extern IntPtr GetCurrentThread();
 [DllImport("advapi32.dll", SetLastError=true)] static extern bool ImpersonateAnonymousToken(IntPtr thread);
 [DllImport("advapi32.dll", SetLastError=true)] static extern bool RevertToSelf();
 [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr CreateFile(string name,uint access,uint share,IntPtr sa,uint creation,uint flags,IntPtr template);
 [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
 public static string Check() {
  string sid=WindowsIdentity.GetCurrent().User.Value;
  string local=@"\\.\pipe\SnooZap.Owner."+sid;
  if (!ImpersonateAnonymousToken(GetCurrentThread())) throw new Exception("Anonymous fixture unavailable: "+Marshal.GetLastWin32Error());
  int denied=0;
  try {
   IntPtr h=CreateFile(local,0xC0000000,0,IntPtr.Zero,3,0x00110000,IntPtr.Zero);
   denied=Marshal.GetLastWin32Error();
   if (h!=new IntPtr(-1)) {CloseHandle(h);throw new Exception("Anonymous pipe client admitted");}
  } finally { if (!RevertToSelf()) Environment.FailFast("Fixture impersonation cleanup failed"); }
  if (denied!=5) throw new Exception("Anonymous access did not fail with access denied: "+denied);
  IntPtr remote=CreateFile(@"\\localhost\pipe\SnooZap.Owner."+sid,0xC0000000,0,IntPtr.Zero,3,0x00110000,IntPtr.Zero);
  int remoteError=Marshal.GetLastWin32Error();
  if (remote!=new IntPtr(-1)) {CloseHandle(remote);throw new Exception("Remote pipe client admitted");}
  if (remoteError!=5) throw new Exception("Remote access could not prove rejection: "+remoteError);
  return "PASS anonymous principal denied; remote UNC client denied; impersonation reverted";
 }
}
'@
[SnooZapPipeAccess]::Check()`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil || !bytes.Contains(data, []byte("PASS anonymous")) {
		t.Fatal("native security fixture failed", err, string(data))
	}
	client := NewClient(Dial)
	defer client.Close(context.Background())
	if _, err := client.Snapshot(ctx); err != nil {
		t.Fatal("denied probes affected legitimate owner access", err)
	}
}
