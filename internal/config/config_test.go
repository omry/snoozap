package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/cli"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/model"
)

func TestEmptyConfigurationNeverCreatesWork(t *testing.T) {
	for _, input := range []string{`{"schema_version":1}`, `{"schema_version":1,"probes":{},"rules":[]}`} {
		cfg, err := config.Parse(strings.NewReader(input))
		if err != nil || cfg.SchemaVersion != 1 || len(cfg.Probes) != 0 || len(cfg.Rules) != 0 {
			t.Fatalf("empty configuration: %+v, %v", cfg, err)
		}
	}
	cfg := config.Empty()
	if cfg.SchemaVersion != 1 || cfg.Probes == nil || cfg.Rules == nil {
		t.Fatal("empty configuration must carry explicit version and empty collections")
	}
}

func TestRejectUnsupportedConfiguration(t *testing.T) {
	cases := map[string]string{
		"empty": "", "null": "null", "array": "[]", "scalar": "true",
		"missing version": `{}`, "old version": `{"schema_version":0}`,
		"future version": `{"schema_version":2}`, "string version": `{"schema_version":"1"}`,
		"fraction version": `{"schema_version":1.5}`, "null version": `{"schema_version":null}`,
		"unknown field":          `{"schema_version":1,"shell":"anything"}`,
		"uppercase version":      `{"SCHEMA_VERSION":1}`,
		"mixed case version":     `{"Schema_Version":1}`,
		"version alias override": `{"schema_version":999,"SCHEMA_VERSION":1}`,
		"version alias first":    `{"SCHEMA_VERSION":1,"schema_version":1}`,
		"uppercase probes":       `{"schema_version":1,"PROBES":{}}`,
		"mixed case probes":      `{"schema_version":1,"Probes":{}}`,
		"probes alias override":  `{"schema_version":1,"probes":null,"PROBES":{}}`,
		"uppercase rules":        `{"schema_version":1,"RULES":[]}`,
		"mixed case rules":       `{"schema_version":1,"Rules":[]}`,
		"rules alias override":   `{"schema_version":1,"rules":[{}],"RULES":[]}`,
		"duplicate version":      `{"schema_version":1,"schema_version":1}`,
		"escaped duplicate":      `{"schema_version":1,"\u0073chema_version":1}`,
		"nested duplicate":       `{"schema_version":1,"probes":{"p":{"type":"process","type":"http"}}}`,
		"probes array":           `{"schema_version":1,"probes":[]}`,
		"rules object":           `{"schema_version":1,"rules":{}}`,
		"null probes":            `{"schema_version":1,"probes":null}`,
		"null rules":             `{"schema_version":1,"rules":null}`,
		"nonempty probes":        `{"schema_version":1,"probes":{"p":{"type":"process"}}}`,
		"nonempty rules":         `{"schema_version":1,"rules":[{}]}`,
		"extra value":            `{"schema_version":1} {}`,
		"trailing garbage":       `{"schema_version":1} garbage`,
		"truncated":              `{"schema_version":1`, "trailing comma": `{"schema_version":1,}`,
		"wrong end": `{"schema_version":1]`,
		"deep":      `{"schema_version":1,"probes":` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + "}",
		"oversized": strings.Repeat(" ", config.MaxBytes+1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Parse(strings.NewReader(input)); err == nil {
				t.Fatal("unsupported input accepted")
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("private input detail") }

func TestReadFailureIsBoundedAndSafe(t *testing.T) {
	_, err := config.Parse(failingReader{})
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe or missing read error: %v", err)
	}
	input := `{"schema_version":1,"secret-value":"do-not-echo"}`
	_, err = config.Parse(strings.NewReader(input))
	if err == nil || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("configuration values must not appear in diagnostics: %v", err)
	}
}

type fakePower struct {
	mu                   sync.Mutex
	calls                []bool
	active               bool
	closed               int
	acquireErr, clearErr error
	closeErr             error
}

func (p *fakePower) SetRequired(ctx context.Context, required bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.calls = append(p.calls, required)
	if required && p.acquireErr != nil {
		return p.acquireErr
	}
	if !required && p.clearErr != nil {
		return p.clearErr
	}
	p.active = required
	return nil
}

func (p *fakePower) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	p.active = false
	return p.closeErr
}

func (p *fakePower) state() ([]bool, bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.calls...), p.active, p.closed
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

func newEngine(t *testing.T, power *fakePower, configError string) *engine.Engine {
	t.Helper()
	e := engine.New(context.Background(), power, fixedClock{time.Unix(123, 0)}, configError)
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func mode(t *testing.T, e *engine.Engine, m model.Mode) model.Snapshot {
	t.Helper()
	s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: m})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestManualControlsWithEmptyConfiguration(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	s, err := e.Snapshot(context.Background())
	if err != nil || s.Mode != model.Auto || s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted {
		t.Fatalf("startup invented protection: %+v %v", s, err)
	}
	if !s.UpdatedAt.Equal(time.Unix(123, 0)) {
		t.Fatal("engine did not use injected clock")
	}
	s = mode(t, e, model.Awake)
	if !s.DesiredHold || !s.NativeRequestActive || !s.NativeRequestAccepted || s.Reason != "Manual Stay awake" {
		t.Fatalf("manual no-setup hold missing: %+v", s)
	}
	sequence := s.Sequence
	for range 50 {
		s, err = e.Snapshot(context.Background())
		if err != nil || s.Sequence != sequence {
			t.Fatal("reading status changed engine state")
		}
	}
	mode(t, e, model.Awake)
	s = mode(t, e, model.AllowSleep)
	if s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted || !strings.Contains(s.Reason, "does not issue Sleep") {
		t.Fatalf("Allow sleep did not release accurately: %+v", s)
	}
	mode(t, e, model.Auto)
	calls, active, _ := p.state()
	if active || len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("unbalanced/repeated native calls: %v active=%t", calls, active)
	}
}

func TestRejectedAcquisitionCannotClaimProtection(t *testing.T) {
	p := &fakePower{acquireErr: errors.New("access denied")}
	e := newEngine(t, p, "")
	s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake})
	if err == nil || s.Mode != model.Awake || !s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted || s.NativeError == "" {
		t.Fatalf("rejected request misrepresented: %+v %v", s, err)
	}
	read, err := e.Snapshot(context.Background())
	if err != nil || read.NativeError == "" || read.NativeRequestAccepted {
		t.Fatal("status must succeed while exposing a rejected power request")
	}
	mode(t, e, model.Auto)
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, closed := p.state()
	if closed != 1 {
		t.Fatal("power handle not closed exactly once")
	}
}

func TestFailedReleaseRemainsVisibleAndExitClosesHandle(t *testing.T) {
	p := &fakePower{clearErr: errors.New("clear failed")}
	e := newEngine(t, p, "")
	mode(t, e, model.Awake)
	s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Auto})
	if err == nil || s.DesiredHold || !s.NativeRequestActive || s.NativeRequestAccepted || s.NativeError == "" {
		t.Fatalf("release failure hidden: %+v %v", s, err)
	}
	if err := e.Close(context.Background()); err == nil {
		t.Fatal("shutdown discarded the release error")
	}
	_, active, closed := p.state()
	if active || closed != 1 {
		t.Fatal("clear failure prevented final handle closure")
	}
	if err := e.Close(context.Background()); err == nil {
		t.Fatal("repeated close lost original cleanup error")
	}
	_, _, closed = p.state()
	if closed != 1 {
		t.Fatal("repeated close duplicated resource cleanup")
	}
}

func TestInvalidStartupConfigurationLeavesManualControlsUsable(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "unsupported configuration schema_version; expected 1")
	s, err := e.Snapshot(context.Background())
	if err != nil || s.ConfigurationError == "" || s.DesiredHold {
		t.Fatal("startup failure missing or created automatic work")
	}
	s = mode(t, e, model.Awake)
	if !s.NativeRequestAccepted || s.ConfigurationError == "" {
		t.Fatal("bad configuration disabled explicit manual control or hid the error")
	}
}

func TestInvalidAndCancelledCommandsHaveNoSideEffect(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	for _, c := range []model.Command{{Operation: "mode", Mode: "invalid"}, {Operation: "shell"}} {
		if _, err := e.Execute(context.Background(), c); err == nil {
			t.Fatal("invalid command accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Execute(ctx, model.Command{Operation: "mode", Mode: model.Awake}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled command accepted")
	}
	calls, active, _ := p.state()
	if len(calls) != 0 || active {
		t.Fatal("invalid/cancelled command changed native state")
	}
}

func TestSnapshotsCannotMutateEngine(t *testing.T) {
	e := newEngine(t, &fakePower{}, "")
	s, _ := e.Snapshot(context.Background())
	s.Limitations[0] = "false external claim"
	s2, _ := e.Snapshot(context.Background())
	if s2.Limitations[0] == s.Limitations[0] {
		t.Fatal("snapshot shares mutable engine data")
	}
}

func TestStopResponseDoesNotClaimAnActiveRequest(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	mode(t, e, model.Awake)
	s, err := e.Execute(context.Background(), model.Command{Operation: "stop"})
	if err != nil || s.NativeRequestActive || s.DesiredHold || s.NativeRequestAccepted || s.Mode != model.Auto {
		t.Fatalf("stop returned stale protection state: %+v %v", s, err)
	}
	if _, err := e.Snapshot(context.Background()); !errors.Is(err, model.ErrStopped) {
		t.Fatal("stopped runtime still accepts status commands")
	}
}

func TestConcurrentControlsRemainSerialized(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 20 {
				m := model.Auto
				if (i+j)%2 == 0 {
					m = model.Awake
				}
				if _, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: m}); err != nil {
					t.Error(err)
				}
				if _, err := e.Snapshot(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, active, closed := p.state()
	for i, v := range calls {
		if v != (i%2 == 0) {
			t.Fatalf("duplicate acquisition or release at %d: %v", i, calls)
		}
	}
	if active || closed != 1 {
		t.Fatal("concurrent work leaked its backend")
	}
}

func TestCancellationAndCloseFailure(t *testing.T) {
	p := &fakePower{closeErr: errors.New("close failed")}
	ctx, cancel := context.WithCancel(context.Background())
	e := engine.New(ctx, p, nil, "")
	mode(t, e, model.Awake)
	cancel()
	deadline, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	for {
		_, err := e.Snapshot(deadline)
		if errors.Is(err, model.ErrStopped) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(deadline); err == nil || err.Error() != "close failed" {
		t.Fatalf("lost close error: %v", err)
	}
	_, active, closed := p.state()
	if active || closed != 1 {
		t.Fatal("cancellation leaked a native resource")
	}
}

func TestConsoleUsesOneServiceAndClosesOnEOF(t *testing.T) {
	p := &fakePower{}
	var out, stderr bytes.Buffer
	starts := 0
	start := func(ctx context.Context, dir string) (model.Service, error) {
		starts++
		if dir != "chosen-directory" {
			t.Fatal("data directory not passed to composition")
		}
		return engine.New(ctx, p, nil, ""), nil
	}
	input := "mode awake\nstatus --json\nstatus --json\n"
	code := cli.Run(context.Background(), []string{"run", "--data-dir", "chosen-directory"}, strings.NewReader(input), &out, &stderr, start)
	if code != 0 || starts != 1 || stderr.Len() != 0 {
		t.Fatalf("console: exit=%d starts=%d stderr=%s", code, starts, stderr.String())
	}
	var statuses []model.Snapshot
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s model.Snapshot
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, s)
	}
	if len(statuses) != 2 || !statuses[0].NativeRequestAccepted || statuses[0].Sequence != statuses[1].Sequence {
		t.Fatalf("bad status output: %s", out.String())
	}
	calls, active, closed := p.state()
	if len(calls) != 2 || !calls[0] || calls[1] || active || closed != 1 {
		t.Fatal("EOF did not balance the shared native request")
	}
}

type awakeNotification struct {
	once  sync.Once
	ready chan struct{}
}

func (w *awakeNotification) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("mode=awake desired=true native_request_active=true accepted=true:")) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestConsoleCancellationWithOpenInputReleasesResources(t *testing.T) {
	p := &fakePower{}
	ctx, cancel := context.WithCancel(context.Background())
	in, commands := io.Pipe()
	t.Cleanup(func() {
		cancel()
		_ = commands.Close()
		_ = in.Close()
	})
	out := &awakeNotification{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"run"}, in, out, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, p, nil, ""), nil
		})
	}()
	if _, err := io.WriteString(commands, "mode awake\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("console did not finish acquiring its power request")
	}
	// Keep input open: cancellation must return without EOF or a stop command.
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cancelled console exited with %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled console remained blocked on open input")
	}
	calls, active, closed := p.state()
	if len(calls) != 2 || !calls[0] || calls[1] || active || closed != 1 {
		t.Fatalf("cancellation did not release exactly once: calls=%v active=%t closed=%d", calls, active, closed)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestConsoleFailuresReleaseResources(t *testing.T) {
	cases := []struct {
		name, input string
		power       *fakePower
		out         io.Writer
		want        int
	}{
		{"unknown command", "shell anything\nstop\n", &fakePower{}, io.Discard, 2},
		{"invalid mode", "mode maybe\n", &fakePower{}, io.Discard, 2},
		{"invalid status", "status --raw\n", &fakePower{}, io.Discard, 2},
		{"oversized input", strings.Repeat("x", 70*1024), &fakePower{}, io.Discard, 1},
		{"broken output", "", &fakePower{}, brokenWriter{}, 1},
		{"rejected request", "mode awake\nstop\n", &fakePower{acquireErr: errors.New("rejected")}, io.Discard, 1},
		{"clear failure", "mode awake\nstop\n", &fakePower{clearErr: errors.New("rejected")}, io.Discard, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := func(ctx context.Context, _ string) (model.Service, error) {
				return engine.New(ctx, tc.power, nil, ""), nil
			}
			code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(tc.input), tc.out, io.Discard, start)
			if code != tc.want {
				t.Fatalf("exit=%d want=%d", code, tc.want)
			}
			_, active, closed := tc.power.state()
			if active || closed != 1 {
				t.Fatal("console failure leaked its runtime")
			}
		})
	}
}

func TestInvalidInvocationNeverStartsOwner(t *testing.T) {
	for _, args := range [][]string{nil, {"mode", "awake"}, {"run", "--unknown"}, {"run", "unexpected"}} {
		started := false
		code := cli.Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string) (model.Service, error) {
			started = true
			return nil, errors.New("must not run")
		})
		if started || code != 2 {
			t.Fatalf("invalid invocation started a runtime: %v", args)
		}
	}
}

func TestOfflineConfigurationCheckAndHelpNeverStartRuntime(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(valid, []byte(`{"schema_version":1,"probes":{},"rules":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte(`{"schema_version":123}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"help"}, 0}, {[]string{"--help"}, 0},
		{[]string{"config", "check", valid}, 0},
		{[]string{"config", "check", invalid}, 2},
		{[]string{"config", "check", filepath.Join(dir, "absent")}, 2},
	} {
		starts := 0
		code := cli.Run(context.Background(), tc.args, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string) (model.Service, error) {
			starts++
			return nil, errors.New("must not acquire a native resource")
		})
		if code != tc.want || starts != 0 {
			t.Fatalf("offline invocation %v: code=%d starts=%d", tc.args, code, starts)
		}
	}
}

type failAfterWriter struct{ remaining int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	w.remaining--
	return len(p), nil
}

func TestOutputFailureWhileHoldingReleasesRequest(t *testing.T) {
	for _, successfulWrites := range []int{1, 2} {
		p := &fakePower{}
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader("mode awake\nstatus --json\n"), &failAfterWriter{successfulWrites}, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, p, nil, ""), nil
		})
		_, active, closed := p.state()
		if code != 1 || active || closed != 1 {
			t.Fatalf("failed output leaked protection: code=%d active=%t closed=%d", code, active, closed)
		}
	}
}

func TestStartupAndInputFailuresAreOperationalErrors(t *testing.T) {
	code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string) (model.Service, error) {
		return nil, errors.New("native resource rejected")
	})
	if code != 1 {
		t.Fatal("native startup failure reported success")
	}
	p := &fakePower{}
	code = cli.Run(context.Background(), []string{"run"}, failingReader{}, io.Discard, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
		return engine.New(ctx, p, nil, "configuration rejected"), nil
	})
	_, active, closed := p.state()
	if code != 1 || active || closed != 1 {
		t.Fatal("input failure did not close its runtime")
	}
}
