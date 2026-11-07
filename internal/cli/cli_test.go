package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/cli"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/model"
)

type power struct{ deny bool }

func (p power) SetRequired(_ context.Context, on bool) error {
	if p.deny && on {
		return errors.New("native denial")
	}
	return nil
}
func (power) Close() error { return nil }

type attached struct {
	model.Service
	closed *int
}

func (s attached) Close(context.Context) error { *s.closed++; return nil }

func fixture(t *testing.T, deny bool) *engine.Engine {
	t.Helper()
	e := engine.New(context.Background(), power{deny}, nil, "", engine.Options{Configuration: config.Empty(), ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }, Save: func(config.Config) error { return nil }})
	t.Cleanup(func() { e.Close(context.Background()) })
	return e
}
func neverStart(t *testing.T) cli.Factory {
	t.Helper()
	return func(context.Context, string) (model.Service, error) {
		t.Error("attached command started an owner")
		return nil, errors.New("unexpected owner")
	}
}
func TestAttachedCommandsShareOneOwnerAndDetach(t *testing.T) {
	e := fixture(t, false)
	closes := 0
	connects := 0
	connector := func(context.Context) (model.Service, error) { connects++; return attached{e, &closes}, nil }
	dir := t.TempDir()
	cfg := filepath.Join(dir, "candidate with spaces.json")
	export := filepath.Join(dir, "export with spaces.json")
	history := filepath.Join(dir, "history with spaces.json")
	if err := os.WriteFile(cfg, []byte(`{"schema_version":1,"probes":{},"rules":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"mode", "awake", "--for", "1h"}, {"status", "--json"}, {"monitor", "off"}, {"history", "--json"}, {"history"}, {"history", "export", history}, {"history", "clear"}, {"config", "apply", cfg}, {"config", "export", export}, {"mode", "allow-sleep"}, {"mode", "auto"},
	} {
		var out, stderr bytes.Buffer
		if code := cli.Run(context.Background(), args, strings.NewReader(""), &out, &stderr, neverStart(t), connector); code != 0 {
			t.Fatal(args, code, stderr.String())
		}
		if _, err := e.Snapshot(context.Background()); err != nil {
			t.Fatal("attached command stopped owner", args, err)
		}
		if args[0] == "status" {
			var s model.Snapshot
			if json.Unmarshal(out.Bytes(), &s) != nil || s.SchemaVersion != 1 || s.Mode != model.Awake || !s.Hold.OSAccepted {
				t.Fatal(out.String())
			}
		}
	}
	if connects != 11 || closes != connects {
		t.Fatal("attached connection lifetime", connects, closes)
	}
	data, err := os.ReadFile(export)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if code := cli.Run(context.Background(), []string{"stop"}, strings.NewReader(""), io.Discard, io.Discard, neverStart(t), connector); code != 0 {
		t.Fatal(code)
	}
	if _, err := e.Snapshot(context.Background()); !errors.Is(err, model.ErrStopped) {
		t.Fatal("stop only detached", err)
	}
}
func TestAttachedExitCodesAndInvalidInputDoNotStartAnOwner(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
	}{{model.ErrNoInstance, 3}, {model.ErrInvalidInput, 2}, {errors.New("operational error"), 1}} {
		code := cli.Run(context.Background(), []string{"status"}, strings.NewReader(""), io.Discard, io.Discard, neverStart(t), func(context.Context) (model.Service, error) { return nil, test.err })
		if code != test.code {
			t.Fatal(code, test.code)
		}
	}
	for _, args := range [][]string{{"mode", "auto", "--for", "1h"}, {"mode", "awake", "--for", "0s"}, {"mode", "awake", "--for", "999999999h"}, {"status", "--raw"}, {"history", "clear", "extra"}, {"monitor", "maybe"}, {"stop", "extra"}, {"config", "apply", "missing.json"}, {"shell", "anything"}} {
		code := cli.Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, neverStart(t), func(context.Context) (model.Service, error) {
			t.Error("invalid command attempted attachment", args)
			return nil, model.ErrNoInstance
		})
		if code != 2 {
			t.Fatal(args, code)
		}
	}
}
func TestStatusSuccessDoesNotClaimNativeProtection(t *testing.T) {
	e := fixture(t, true)
	_, _ = e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake})
	closes := 0
	var out bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "--json"}, strings.NewReader(""), &out, io.Discard, neverStart(t), func(context.Context) (model.Service, error) { return attached{e, &closes}, nil })
	var s model.Snapshot
	if code != 0 || json.Unmarshal(out.Bytes(), &s) != nil || !s.Hold.Requested || s.Hold.OSAccepted || len(s.Limitations) == 0 {
		t.Fatal(code, out.String())
	}
}
func TestOfflineCheckNeverConnectsAndReportsResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"probes":{},"rules":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := cli.Run(context.Background(), []string{"config", "check", path}, strings.NewReader(""), &out, io.Discard, neverStart(t), func(context.Context) (model.Service, error) {
		t.Error("offline check contacted owner")
		return nil, model.ErrNoInstance
	})
	if code != 0 || !strings.Contains(out.String(), "Configuration valid") {
		t.Fatal(code, out.String())
	}
}
func TestForegroundConsoleExitsWhenAttachedCallerStopsOwner(t *testing.T) {
	e := fixture(t, false)
	in, writer := io.Pipe()
	defer writer.Close()
	defer in.Close()
	ready := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(context.Background(), []string{"run"}, in, io.Discard, io.Discard, func(context.Context, string) (model.Service, error) { close(ready); return e, nil })
	}()
	<-ready
	if _, err := e.Execute(context.Background(), model.Command{Operation: "stop"}); err != nil {
		t.Fatal(err)
	}
	// Startup can race stop: either a clean observed exit or an operational
	// startup observation error is truthful, and neither may retain a hold.
	var code int
	select {
	case code = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground console retained stopped owner")
	}
	if code != 0 && code != 1 {
		t.Fatal(code)
	}
}
func TestWakeUnavailableDoesNotCreateFutureAuthority(t *testing.T) {
	e := fixture(t, false)
	closes := 0
	for _, args := range [][]string{{"wake", "list"}, {"wake", "send", "remembered-id"}, {"tui"}} {
		var stderr bytes.Buffer
		code := cli.Run(context.Background(), args, strings.NewReader(""), io.Discard, &stderr, neverStart(t), func(context.Context) (model.Service, error) { return attached{e, &closes}, nil })
		if code != 1 || !strings.Contains(stderr.String(), "unavailable") {
			t.Fatal(code, stderr.String())
		}
	}
	if code := cli.Run(context.Background(), []string{"run", "--tui"}, strings.NewReader(""), io.Discard, io.Discard, neverStart(t)); code != 1 {
		t.Fatal("unsupported TUI started or silently substituted console", code)
	}
}
