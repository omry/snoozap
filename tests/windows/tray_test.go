//go:build windows

package windows_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"github.com/omry/snoozap/internal/app"
	"github.com/omry/snoozap/internal/desktop"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
	native "github.com/omry/snoozap/internal/platform/windows"
)

func TestTrayStatusSeparatesSelectedModeFromAcceptedProtection(t *testing.T) {
	accepted := model.Snapshot{SchemaVersion: 1, Sequence: 1, Mode: model.Awake, Activity: model.Inactive, DesiredHold: true, NativeRequestActive: true, NativeRequestAccepted: true, Hold: model.Hold{Requested: true, OSAccepted: true}, Reason: "Manual Stay awake"}
	for _, tc := range []struct {
		name, title, icon string
		change            func(*desktop.State)
	}{
		{"accepted", "Keeping awake", "accepted", func(*desktop.State) {}},
		{"request-rejected", "Sleep prevention failed", "failed", func(s *desktop.State) { s.Snapshot.NativeError = "Windows power request failed" }},
		{"battery-cutoff", "Sleep prevention paused", "paused", func(s *desktop.State) {
			s.Snapshot.Battery = model.Battery{Blocked: true, Reason: "Battery floor protection"}
		}},
		{"desired-only", "Protection unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot.Hold.OSAccepted = false }},
		{"native-not-accepted", "Protection unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot.NativeRequestAccepted = false }},
		{"native-not-active", "Protection unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot.NativeRequestActive = false }},
		{"disconnected", "Owner unavailable", "unknown", func(s *desktop.State) { s.Connected = false }},
		{"allow-sleep", "Normal sleep allowed", "allowed", func(s *desktop.State) { s.Snapshot = model.Snapshot{Mode: model.AllowSleep, Activity: model.Active} }},
		{"inactive", "Normal sleep allowed", "allowed", func(s *desktop.State) { s.Snapshot = model.Snapshot{Mode: model.Auto, Activity: model.Inactive} }},
		{"startup-unknown", "Activity unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot = model.Snapshot{Mode: model.Auto, Activity: model.Unknown} }},
		{"invalid-config", "Activity unconfirmed", "unknown", func(s *desktop.State) {
			s.Snapshot = model.Snapshot{Mode: model.Auto, Activity: model.Inactive, ConfigurationError: "Startup configuration rejected"}
		}},
		{"retained-unknown", "Keeping awake", "accepted", func(s *desktop.State) { s.Snapshot.Mode, s.Snapshot.Activity = model.Auto, model.Unknown }},
		{"startup-hold-unknown", "Keeping awake", "accepted", func(s *desktop.State) {
			s.Snapshot.Mode, s.Snapshot.Activity = model.Auto, model.Unknown
			s.Snapshot.Rules = []model.RuleStatus{{ID: "work", State: model.Unknown, Startup: true}}
		}},
		{"mixed-hold-unknown", "Keeping awake", "accepted", func(s *desktop.State) {
			s.Snapshot.Mode, s.Snapshot.Activity = model.Auto, model.Unknown
			s.Snapshot.Rules = []model.RuleStatus{
				{ID: "startup", State: model.Unknown, Startup: true},
				{ID: "retained", State: model.Unknown, Retained: true},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := desktop.State{Snapshot: accepted, Connected: true}
			tc.change(&s)
			p := desktop.Describe(s)
			if p.Title != tc.title || p.Icon != tc.icon {
				t.Fatalf("presentation: %+v", p)
			}
			if tc.name == "accepted" && (!strings.Contains(p.Request, "Windows accepted") || !strings.Contains(p.Request, "lid")) {
				t.Fatal("accepted API result lost its platform limitation", p)
			}
			if tc.name == "retained-unknown" && !strings.Contains(p.Reason, "unconfirmed") {
				t.Fatal("retained work presented as freshly confirmed", p)
			}
			if tc.name == "startup-hold-unknown" && (!strings.Contains(p.Reason, "startup") || strings.Contains(p.Reason, "last confirmed")) {
				t.Fatal("startup protection was presented as retained work", p)
			}
			if tc.name == "mixed-hold-unknown" && (!strings.Contains(p.Reason, "startup") || !strings.Contains(p.Reason, "last confirmed")) {
				t.Fatal("mixed startup and retained causes were not both presented", p)
			}
		})
	}
	limitations := desktop.Describe(desktop.State{Connected: true, Snapshot: acceptedSnapshotWithLimitations()})
	if len(limitations.Limitations) != len(runtimeLimitations()) {
		t.Fatal("runtime limitations were not retained in presentation", limitations)
	}
	for _, limitation := range limitations.Limitations {
		if len(utf16.Encode([]rune(limitation))) > 120 || strings.IndexFunc(limitation, unicode.IsControl) >= 0 {
			t.Fatal("runtime limitations were unsafe or overlong", limitations)
		}
	}
	deadline := time.Now().Add(time.Minute)
	p := desktop.Describe(desktop.State{Connected: true, Snapshot: model.Snapshot{Mode: model.Awake, Deadline: &deadline, RemainingMS: 60001, Reason: "Manual"}})
	if !strings.Contains(p.Selected, "1m 01s remaining; then Auto") {
		t.Fatal("timer rounding or expiry destination", p)
	}
	p = desktop.Describe(desktop.State{Message: strings.Repeat("\U0001f600\n\t\x00", 100)})
	if len(utf16.Encode([]rune(p.Tooltip))) > 120 || strings.IndexFunc(p.Tooltip+p.Reason, unicode.IsControl) >= 0 {
		t.Fatal("unsafe or overlong native text")
	}
}

func acceptedSnapshotWithLimitations() model.Snapshot {
	return model.Snapshot{
		SchemaVersion:         1,
		Mode:                  model.Awake,
		Activity:              model.Inactive,
		DesiredHold:           true,
		NativeRequestActive:   true,
		NativeRequestAccepted: true,
		Hold:                  model.Hold{Requested: true, OSAccepted: true},
		Reason:                "Manual Stay awake",
		Limitations:           runtimeLimitations(),
	}
}

func runtimeLimitations() []string {
	return []string{
		"An accepted request does not guarantee protection from explicit Sleep or lid close.",
		"Windows Modern Standby and battery policies can limit SystemRequired requests.",
		"SnooZap does not request that the display stay on.",
		"Periodic resume detection unavailable; native notifications remain active.",
	}
}

type failingSuspendClock struct{ origin time.Time }

type failingSuspendTimer struct{ *time.Timer }

func (t failingSuspendTimer) C() <-chan time.Time { return t.Timer.C }

func (c failingSuspendClock) Now() time.Time { return time.Now() }

func (c failingSuspendClock) Elapsed() time.Duration { return time.Since(c.origin) }

func (c failingSuspendClock) NewTimer(d time.Duration) model.Timer {
	return failingSuspendTimer{time.NewTimer(d)}
}

func (failingSuspendClock) SleepTime() (time.Duration, error) {
	return 0, errors.New("fixture suspend clock unavailable")
}

func TestActualEngineLimitationsRemainVisible(t *testing.T) {
	clock := failingSuspendClock{origin: time.Now()}
	service := engine.New(context.Background(), &trayPower{}, clock, "", engine.Options{
		ReadPower: func(context.Context) (model.Battery, error) {
			return model.Battery{Known: true, Source: "ac"}, nil
		},
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assertLimitations := func(label string, snapshot model.Snapshot) {
		if len(snapshot.Limitations) != len(runtimeLimitations()) {
			t.Fatalf("%s engine limitations: %#v", label, snapshot.Limitations)
		}
		presentation := desktop.Describe(desktop.State{Connected: true, Snapshot: snapshot})
		for _, limitation := range runtimeLimitations() {
			found := false
			for _, rendered := range presentation.Limitations {
				if strings.Contains(rendered, limitation) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s presentation omitted %q: %q", label, limitation, presentation.Limitations)
			}
		}
		for _, limitation := range presentation.Limitations {
			if len(utf16.Encode([]rune(limitation))) > 120 || strings.IndexFunc(limitation, unicode.IsControl) >= 0 {
				t.Fatalf("%s presentation is unsafe or overlong: %q", label, limitation)
			}
		}
	}
	auto, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal("actual engine Auto snapshot", err)
	}
	assertLimitations("Auto", auto)
	if auto.NativeRequestAccepted {
		t.Fatalf("Auto snapshot unexpectedly accepted protection: %+v", auto)
	}
	if got := desktop.Describe(desktop.State{Connected: true, Snapshot: auto}).Request; got != "No accepted sleep-prevention request" {
		t.Fatalf("Auto presentation blurred acceptance distinction: %q", got)
	}
	manual, err := service.Execute(ctx, model.Command{Operation: "mode", Mode: model.Awake})
	if err != nil {
		t.Fatal("actual engine manual snapshot", err)
	}
	assertLimitations("manual", manual)
	if !manual.NativeRequestAccepted {
		t.Fatalf("manual snapshot did not retain accepted protection: %+v", manual)
	}
	if got := desktop.Describe(desktop.State{Connected: true, Snapshot: manual}).Request; !strings.Contains(got, "Windows accepted") {
		t.Fatalf("manual presentation lost acceptance distinction: %q", got)
	}
	unsafe := manual
	unsafe.Limitations = []string{"unsafe\ntext\x00 &"}
	presentation := desktop.Describe(desktop.State{Connected: true, Snapshot: unsafe})
	if len(presentation.Limitations) != 1 || strings.IndexFunc(presentation.Limitations[0], unicode.IsControl) >= 0 || !strings.Contains(presentation.Limitations[0], " &") {
		t.Fatalf("limitation sanitization or ampersand preservation failed: %q", presentation.Limitations)
	}
}

type trayPower struct {
	mu       sync.Mutex
	required bool
	closed   int
	fail     atomic.Bool
}

func (p *trayPower) SetRequired(_ context.Context, required bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail.Load() {
		return errors.New("fixture native rejection")
	}
	p.required = required
	return nil
}
func (p *trayPower) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.required = false
	p.closed++
	return nil
}

func trayUpdates() (chan desktop.State, func(desktop.State)) {
	updates := make(chan desktop.State, 32)
	return updates, func(s desktop.State) {
		select {
		case updates <- s:
		default:
		}
	}
}

func waitTray(t *testing.T, updates <-chan desktop.State, predicate func(desktop.State) bool) desktop.State {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case s := <-updates:
			if predicate(s) {
				return s
			}
		case <-deadline.C:
			t.Fatal("tray did not reach the required state")
		}
	}
}

func TestTrayControllerModesTimersErrorsAndOwnedCleanup(t *testing.T) {
	power := &trayPower{}
	service := engine.New(context.Background(), power, nil, "", engine.Options{ReadPower: func(context.Context) (model.Battery, error) {
		return model.Battery{Source: "ac", Known: true}, nil
	}})
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	for _, mode := range []model.Mode{model.Awake, model.AllowSleep, model.Auto} {
		if err := c.Execute(model.Command{Operation: "mode", Mode: mode}); err != nil {
			t.Fatal(err)
		}
		s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.Mode == mode })
		if s.Snapshot.NativeRequestAccepted != (mode == model.Awake) {
			t.Fatal("mode did not reach actual engine power state", s)
		}
	}
	for _, duration := range []time.Duration{time.Hour, 60 * time.Millisecond} {
		if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake, Duration: duration}); err != nil {
			t.Fatal(err)
		}
		waitTray(t, updates, func(s desktop.State) bool {
			return !s.Pending && s.Snapshot.Deadline != nil && s.Snapshot.RemainingMS <= duration.Milliseconds()
		})
	}
	waitTray(t, updates, func(s desktop.State) bool {
		return s.Snapshot.Mode == model.Auto && s.Snapshot.Deadline == nil && !s.Snapshot.Hold.Requested
	})
	if err := c.Execute(model.Command{Operation: "config"}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatal("tray controller dispatched an unrelated command", err)
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: "invalid"}); err == nil {
		t.Fatal("invalid mode accepted")
	}
	power.fail.Store(true)
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err == nil {
		t.Fatal("fixture OS rejection hidden")
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.NativeError != "" })
	if desktop.Describe(s).Title != "Sleep prevention failed" || s.Snapshot.Hold.OSAccepted {
		t.Fatal("selected mode overclaimed protection", s)
	}
	power.fail.Store(false)
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal("repeated close", err)
	}
	power.mu.Lock()
	defer power.mu.Unlock()
	if power.required || power.closed != 1 {
		t.Fatal("owned power lifetime unbalanced", power.required, power.closed)
	}
}

type trayBlockingService struct {
	model.Service
	started chan struct{}
	count   atomic.Int32
}

func (s *trayBlockingService) Execute(ctx context.Context, _ model.Command) (model.Snapshot, error) {
	s.count.Add(1)
	close(s.started)
	<-ctx.Done()
	return model.Snapshot{}, ctx.Err()
}

func TestTrayControllerDoesNotQueueOrReplayUncertainCommands(t *testing.T) {
	service := &trayBlockingService{Service: engine.New(context.Background(), &trayPower{}, nil, ""), started: make(chan struct{})}
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	result := make(chan error, 1)
	go func() { result <- c.Execute(model.Command{Operation: "mode", Mode: model.Awake}) }()
	select {
	case <-service.started:
	case <-time.After(time.Second):
		t.Fatal("command not dispatched")
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.AllowSleep}); !errors.Is(err, model.ErrUnavailable) {
		t.Fatal("second mutation queued", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) || service.count.Load() != 1 {
		t.Fatal("uncertain command replayed", err, service.count.Load())
	}
}

type trayDelayedService struct {
	model.Service
	updates          chan model.Snapshot
	started, release chan struct{}
	subscribeError   bool
}

func (s *trayDelayedService) Subscribe(context.Context) (<-chan model.Snapshot, error) {
	if s.subscribeError {
		return nil, model.ErrUnavailable
	}
	return s.updates, nil
}

func (s *trayDelayedService) Execute(ctx context.Context, _ model.Command) (model.Snapshot, error) {
	close(s.started)
	select {
	case <-ctx.Done():
		return model.Snapshot{}, ctx.Err()
	case <-s.release:
		return model.Snapshot{SchemaVersion: 1, Sequence: 2, Mode: model.Awake, DesiredHold: true,
			NativeRequestActive: true, NativeRequestAccepted: true, Hold: model.Hold{Requested: true, OSAccepted: true}}, nil
	}
}

func (*trayDelayedService) Close(context.Context) error { return nil }

func TestTrayControllerLateReplyCannotRestoreTerminatedSubscription(t *testing.T) {
	for _, setupFailure := range []bool{false, true} {
		name := "disconnect"
		if setupFailure {
			name = "subscribe-failure"
		}
		t.Run(name, func(t *testing.T) {
			service := &trayDelayedService{updates: make(chan model.Snapshot, 1), started: make(chan struct{}), release: make(chan struct{}), subscribeError: setupFailure}
			service.updates <- model.Snapshot{SchemaVersion: 1, Sequence: 1, Mode: model.Auto}
			updates, update := trayUpdates()
			c := desktop.NewController(context.Background(), service, update)
			t.Cleanup(func() { _ = c.Close() })
			if setupFailure {
				waitTray(t, updates, func(s desktop.State) bool { return !s.Connected && s.Message != "" })
			} else {
				waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
			}
			result := make(chan error, 1)
			go func() { result <- c.Execute(model.Command{Operation: "mode", Mode: model.Awake}) }()
			select {
			case <-service.started:
			case <-time.After(time.Second):
				t.Fatal("command did not start")
			}
			if !setupFailure {
				close(service.updates)
				waitTray(t, updates, func(s desktop.State) bool { return !s.Connected })
			}
			close(service.release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending })
			if s.Connected || desktop.Describe(s).Title != "Owner unavailable" || desktop.Describe(s).Icon == "accepted" {
				t.Fatal("late reply restored terminated observation", s, desktop.Describe(s))
			}
		})
	}
}

func TestNativeTrayCommandFailureKeepsHealthySubscription(t *testing.T) {
	requireNative(t)
	owner := launch(t, executable(t), t.TempDir())
	var failNext atomic.Bool
	client := ipc.NewClient(func(ctx context.Context) (ipc.Conn, error) {
		if failNext.Swap(false) {
			return nil, model.ErrUnavailable
		}
		return ipc.Dial(ctx)
	})
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), client, update)
	t.Cleanup(func() { _ = c.Close() })
	before := waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	failNext.Store(true)
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); !errors.Is(err, model.ErrUnavailable) {
		t.Fatal("fixture command failure was not returned", err)
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Message != "" })
	if !s.Connected || s.Snapshot.Sequence != before.Snapshot.Sequence || s.Snapshot.Mode != before.Snapshot.Mode {
		t.Fatal("command failure lost a healthy subscription's unchanged status", before, s)
	}
	if desktop.Describe(s).Title == "Owner unavailable" || !strings.Contains(s.Message, "not confirmed") {
		t.Fatal("command uncertainty was presented as lost connectivity", s)
	}
	if current := owner.status(t, ""); current.Sequence != before.Snapshot.Sequence || current.Mode != before.Snapshot.Mode {
		t.Fatal("failed command changed the owner state", current)
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal("a fresh command could not use the healthy owner", err)
	}
	waitTray(t, updates, func(s desktop.State) bool {
		return s.Connected && !s.Pending && s.Message == "" && s.Snapshot.Mode == model.Awake
	})
}

func TestNativeTrayControllerAttachesAndDetachPreservesConsoleOwner(t *testing.T) {
	requireNative(t)
	owner := launch(t, executable(t), t.TempDir())
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), ipc.NewClient(ipc.Dial), update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.NativeRequestAccepted })
	fromConsole := owner.status(t, "")
	if fromConsole.Mode != s.Snapshot.Mode || !fromConsole.NativeRequestAccepted {
		t.Fatal("console and attached tray disagree")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !owner.status(t, "").NativeRequestAccepted {
		t.Fatal("closing an attached view stopped the console owner")
	}
	observations, observe := trayUpdates()
	attached := desktop.NewController(context.Background(), ipc.NewClient(ipc.Dial), observe)
	t.Cleanup(func() { _ = attached.Close() })
	waitTray(t, observations, func(s desktop.State) bool { return s.Connected && s.Snapshot.NativeRequestAccepted })
	owner.stop(t)
	s = waitTray(t, observations, func(s desktop.State) bool { return !s.Connected })
	if desktop.Describe(s).Title != "Owner unavailable" {
		t.Fatal("owner disconnect retained the accepted view", s)
	}
}

func TestNativeTrayOwnedExternalStopReleasesOwner(t *testing.T) {
	requireNative(t)
	trayExe := os.Getenv("SNOOZAP_TRAY_EXE")
	consoleExe := os.Getenv("SNOOZAP_CONSOLE_EXE")
	if trayExe == "" || consoleExe == "" {
		t.Skip("SNOOZAP_TRAY_EXE and SNOOZAP_CONSOLE_EXE must name the built Windows fixtures")
	}
	if !filepath.IsAbs(trayExe) || !filepath.IsAbs(consoleExe) {
		t.Fatal("native fixture paths must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	localAppData := t.TempDir()
	fixtureEnv := func() []string {
		env := make([]string, 0, len(os.Environ())+1)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(entry), "LOCALAPPDATA=") {
				env = append(env, entry)
			}
		}
		return append(env, "LOCALAPPDATA="+localAppData)
	}
	cli := func(args ...string) (int, string) {
		cmd := exec.CommandContext(ctx, consoleExe, args...)
		cmd.Env = fixtureEnv()
		output, err := cmd.CombinedOutput()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode(), string(output)
			}
			t.Fatal("console fixture command failed to launch", err)
		}
		return 0, string(output)
	}
	status := func() (int, model.Snapshot, string) {
		code, output := cli("status", "--json")
		var snapshot model.Snapshot
		if code == 0 {
			if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
				t.Fatalf("status output was not JSON: %v (%q)", err, output)
			}
		}
		return code, snapshot, output
	}
	waitStatus := func(wantCode int) model.Snapshot {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			code, snapshot, output := status()
			if code == wantCode {
				return snapshot
			}
			if code != 0 && wantCode != 0 && code != 3 {
				t.Fatalf("unexpected status code=%d output=%q", code, output)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("status did not reach exit code %d", wantCode)
		return model.Snapshot{}
	}
	start := func(label string) (*exec.Cmd, *os.File) {
		log, err := os.Create(filepath.Join(t.TempDir(), label+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, trayExe)
		cmd.Env = fixtureEnv()
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal("tray fixture failed to start", err)
		}
		return cmd, log
	}
	waitExit := func(cmd *exec.Cmd) {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("tray fixture exited with error: %v", err)
		}
	}
	if code, _, output := status(); code != 3 {
		t.Fatalf("owner preflight was not no-instance: code=%d output=%q", code, output)
	}
	tray, trayLog := start("owned-stop")
	trayWaited := false
	t.Cleanup(func() {
		if trayLog != nil {
			trayLog.Close()
		}
		if !trayWaited && tray.ProcessState == nil {
			_, _ = cli("stop")
			_ = tray.Wait()
		}
	})
	initial := waitStatus(0)
	if initial.Mode != model.Auto || initial.NativeRequestActive || initial.ConfigurationError != "" {
		t.Fatalf("empty tray owner was not safe Auto: %+v", initial)
	}
	if code, output := cli("stop"); code != 0 {
		t.Fatalf("external stop failed: code=%d output=%q", code, output)
	}
	waitExit(tray)
	trayWaited = true
	if code, _, output := status(); code != 3 {
		t.Fatalf("status after external stop was not no-instance: code=%d output=%q", code, output)
	}
	fresh, freshLog := start("fresh-owner")
	freshWaited := false
	t.Cleanup(func() {
		if freshLog != nil {
			freshLog.Close()
		}
		if !freshWaited && fresh.ProcessState == nil {
			_, _ = cli("stop")
			_ = fresh.Wait()
		}
	})
	freshStatus := waitStatus(0)
	if freshStatus.Mode != model.Auto {
		t.Fatalf("fresh owner did not start in Auto: %+v", freshStatus)
	}
	if code, output := cli("stop"); code != 0 {
		t.Fatalf("fresh owner external stop failed: code=%d output=%q", code, output)
	}
	waitExit(fresh)
	freshWaited = true
	if code, _, output := status(); code != 3 {
		t.Fatalf("final status was not no-instance: code=%d output=%q", code, output)
	}
}

func TestNativeTraySharedCompositionIsSingleAndReleasesOwner(t *testing.T) {
	requireNative(t)
	service, err := app.Start(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	second, err := app.Start(context.Background(), t.TempDir())
	if second != nil || !errors.Is(err, native.ErrAlreadyRunning) {
		t.Fatal("second composition bypassed the user owner", second, err)
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	waitTray(t, updates, func(s desktop.State) bool { return s.Snapshot.NativeRequestAccepted })
	if err := c.Execute(model.Command{Operation: "stop"}); err != nil {
		t.Fatal("Quit did not stop the actual shared runtime", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := native.AcquireOwner()
	if err != nil {
		t.Fatal("shutdown retained native ownership", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

var trayUser32 = syscall.NewLazyDLL("user32.dll")

func trayCall(name string, args ...uintptr) uintptr {
	r, _, _ := trayUser32.NewProc(name).Call(args...)
	return r
}

// Find only a window created by this exact fixture process. HWND_MESSAGE (-3)
// selects message-only tray windows; native popup menus are top-level windows.
func trayWindow(pid int, parent uintptr, class string) uintptr {
	name, _ := syscall.UTF16PtrFromString(class)
	var previous uintptr
	for {
		hwnd := trayCall("FindWindowExW", parent, previous, uintptr(unsafe.Pointer(name)), 0)
		if hwnd == 0 {
			return 0
		}
		var owner uint32
		trayCall("GetWindowThreadProcessId", hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) == pid {
			return hwnd
		}
		previous = hwnd
	}
}

func trayEventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(description)
}

func trayMenuLabel(menu, index uintptr) string {
	var text [256]uint16
	trayCall("GetMenuStringW", menu, index, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)), 0x400)
	return syscall.UTF16ToString(text[:])
}

func trayMenuItemID(menu uintptr, label string) uintptr {
	for index := uintptr(0); index < 32; index++ {
		if trayMenuLabel(menu, index) != label {
			continue
		}
		id := trayCall("GetMenuItemID", menu, index)
		if id != ^uintptr(0) && id != 0xffffffff {
			return id
		}
	}
	return ^uintptr(0)
}

const nativeLimitationRows = 8

func trayMenuContainsLimitations(menu uintptr, warnings []string) bool {
	for _, warning := range warnings {
		found := false
		for index := uintptr(10); index < 10+nativeLimitationRows; index++ {
			if strings.Contains(trayMenuLabel(menu, index), warning) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestNativeTrayShellKeyboardSelectionOpensMenu(t *testing.T) {
	requireNative(t)
	exe := os.Getenv("SNOOZAP_TRAY_EXE")
	if exe == "" {
		t.Skip("SNOOZAP_TRAY_EXE must name the built native tray")
	}
	if !filepath.IsAbs(exe) {
		t.Fatal("SNOOZAP_TRAY_EXE must be absolute")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "LOCALAPPDATA=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LOCALAPPDATA="+t.TempDir())
	log, err := os.Create(filepath.Join(t.TempDir(), "tray-keyselect.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	var hwnd uintptr
	trayEventually(t, "fixture tray window unavailable", func() bool {
		hwnd = trayWindow(cmd.Process.Pid, ^uintptr(2), "SnooZapTrayWindow")
		return hwnd != 0
	})
	// NIN_KEYSELECT is delivered as the low word of the tray callback. It is
	// the shell's ENTER/SPACE activation path, distinct from mouse callbacks.
	if trayCall("PostMessageW", hwnd, 0x401, 0, 0x401) == 0 {
		t.Fatal("fixture keyboard-selection callback failed")
	}
	var popup, menu uintptr
	trayEventually(t, "shell keyboard selection did not open the native menu", func() bool {
		popup = trayWindow(cmd.Process.Pid, 0, "#32768")
		if popup != 0 {
			menu = trayCall("SendMessageW", popup, 0x1e1, 0, 0)
		}
		return menu != 0 && strings.Contains(trayMenuLabel(menu, 0), "SnooZap")
	})
	quit := trayMenuItemID(menu, "Quit SnooZap")
	if quit == ^uintptr(0) || quit == 0xffffffff {
		t.Fatal("native Quit command not found after keyboard activation")
	}
	trayCall("PostMessageW", hwnd, 0x1f, 0, 0)
	trayCall("PostMessageW", hwnd, 0x111, quit, 0)
	if err := cmd.Wait(); err != nil {
		t.Fatal("native Quit after keyboard activation failed", err)
	}
}

func TestNativeTrayProcessMenuActivationAndQuit(t *testing.T) {
	requireNative(t)
	exe := os.Getenv("SNOOZAP_TRAY_EXE")
	if exe == "" {
		t.Skip("SNOOZAP_TRAY_EXE must name the built native tray")
	}
	if !filepath.IsAbs(exe) {
		t.Fatal("SNOOZAP_TRAY_EXE must be absolute")
	}
	for _, attached := range []bool{false, true} {
		name := "tray-owned"
		if attached {
			name = "console-owned"
		}
		t.Run(name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			previousDPI := trayCall("SetThreadDpiAwarenessContext", ^uintptr(3)) // Per-monitor V2, like Wails.
			if previousDPI == 0 {
				t.Fatal("fixture DPI context could not be set")
			}
			defer trayCall("SetThreadDpiAwarenessContext", previousDPI)
			var console *consoleProcess
			if attached {
				console = launch(t, executable(t), t.TempDir())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(entry), "LOCALAPPDATA=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "LOCALAPPDATA="+t.TempDir())
			log, err := os.Create(filepath.Join(t.TempDir(), "tray.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			var hwnd uintptr
			trayEventually(t, "fixture tray window unavailable", func() bool {
				hwnd = trayWindow(cmd.Process.Pid, ^uintptr(2), "SnooZapTrayWindow")
				return hwnd != 0
			})
			client := ipc.NewClient(ipc.Dial)
			defer client.Close(context.Background())
			status := func(mode model.Mode) model.Snapshot {
				var s model.Snapshot
				trayEventually(t, "native menu did not apply "+string(mode), func() bool {
					var err error
					s, err = client.Snapshot(ctx)
					return err == nil && s.Mode == mode
				})
				return s
			}
			if s := status(model.Auto); s.NativeRequestActive || s.ConfigurationError != "" {
				t.Fatal("empty tray startup overclaimed or failed", s)
			}
			open := func(event uintptr) (uintptr, uintptr) {
				if trayCall("PostMessageW", hwnd, 0x401, 0, event) == 0 {
					t.Fatal("fixture tray callback failed")
				}
				var popup, menu uintptr
				trayEventually(t, "fixture native popup unavailable", func() bool {
					popup = trayWindow(cmd.Process.Pid, 0, "#32768")
					if popup != 0 {
						menu = trayCall("SendMessageW", popup, 0x1e1, 0, 0)
					}
					return menu != 0
				})
				return popup, menu
			}
			popup, menu := open(0x202) // The shell's primary-button callback.
			trayEventually(t, "native menu does not reflect the empty engine snapshot", func() bool {
				return strings.Contains(trayMenuLabel(menu, 0), "Normal sleep allowed") && trayMenuLabel(menu, 5) == "Auto" &&
					trayMenuContainsLimitations(menu, runtimeLimitations()[:3])
			})
			// Navigate by the menu's actual highlighted row. Windows may include
			// disabled status rows in keyboard navigation.
			for i := 0; i < 16; i++ {
				trayCall("PostMessageW", popup, 0x100, 0x28, 1)
				time.Sleep(30 * time.Millisecond)
				state := trayCall("GetMenuState", menu, 6, 0x400)
				if state&0x80 != 0 {
					trayCall("PostMessageW", popup, 0x100, 0x0d, 1)
					break
				}
				if i == 15 {
					t.Fatalf("keyboard did not highlight Stay awake: state=%x foreground=%x popup=%x owner=%x", state, trayCall("GetForegroundWindow"), popup, hwnd)
				}
			}
			if s := status(model.Awake); !s.NativeRequestAccepted {
				t.Fatal("native menu selected awake without accepted protection", s)
			}
			second := exec.CommandContext(ctx, exe, "mode", "allow-sleep")
			second.Env = cmd.Env
			if out, err := second.CombinedOutput(); err != nil {
				t.Fatal("repeated tray launch failed", err, string(out))
			}
			if s := status(model.Awake); !s.NativeRequestAccepted {
				t.Fatal("activation payload mutated or stopped the runtime", s)
			}
			// Repeated launch opens the existing menu; read native command IDs
			// from that menu rather than assuming framework-assigned IDs.
			trayEventually(t, "activation did not open the existing menu", func() bool {
				popup := trayWindow(cmd.Process.Pid, 0, "#32768")
				if popup != 0 {
					menu = trayCall("SendMessageW", popup, 0x1e1, 0, 0)
				}
				return popup != 0 && menu != 0
			})
			if !strings.Contains(trayMenuLabel(menu, 0), "Keeping awake") {
				t.Fatal("native view did not show accepted protection", trayMenuLabel(menu, 0))
			}
			invoke := func(current, index uintptr) {
				id := trayCall("GetMenuItemID", current, index)
				if id == ^uintptr(0) || id == 0xffffffff {
					t.Fatal("native command not found")
				}
				trayCall("PostMessageW", hwnd, 0x1f, 0, 0) // Cancel only this fixture's popup.
				trayCall("PostMessageW", hwnd, 0x111, id, 0)
			}
			presets := trayCall("GetSubMenu", menu, 7)
			if trayMenuLabel(presets, 0) != "15 minutes" || trayMenuLabel(presets, 2) != "4 hours" {
				t.Fatal("timed native menu presets missing")
			}
			invoke(presets, 1)
			trayEventually(t, "native timed action did not set its deadline", func() bool {
				s, err := client.Snapshot(ctx)
				return err == nil && s.Deadline != nil && s.RemainingMS > 59*time.Minute.Milliseconds() && s.RemainingMS <= time.Hour.Milliseconds()
			})
			popup, menu = open(0x7b) // The shell's context-menu callback.
			trayEventually(t, "native Allow sleep row remains disabled", func() bool {
				return trayCall("GetMenuState", menu, 8, 0x400)&3 == 0
			})
			if trayCall("HiliteMenuItem", hwnd, menu, 6, 0x480) == 0 {
				t.Fatal("native timed menu could not highlight a mode")
			}
			previous, updates := trayMenuLabel(menu, 2), 0
			trayEventually(t, "native menu did not render three countdown changes", func() bool {
				if trayCall("GetMenuState", menu, 6, 0x400)&0x80 == 0 {
					t.Fatal("countdown updates cleared the native keyboard highlight")
				}
				if current := trayMenuLabel(menu, 2); current != previous {
					previous = current
					updates++
				}
				return updates >= 3
			})
			var rect struct{ Left, Top, Right, Bottom int32 }
			if trayCall("GetMenuItemRect", 0, menu, 8, uintptr(unsafe.Pointer(&rect))) == 0 {
				t.Fatal("native Allow sleep row has no mouse target")
			}
			point := struct{ X, Y int32 }{(rect.Left + rect.Right) / 2, (rect.Top + rect.Bottom) / 2}
			var saved struct{ X, Y int32 }
			if trayCall("GetCursorPos", uintptr(unsafe.Pointer(&saved))) == 0 {
				t.Fatal("could not save the mouse position")
			}
			defer func() {
				var current struct{ X, Y int32 }
				if trayCall("GetCursorPos", uintptr(unsafe.Pointer(&current))) != 0 && current == point {
					trayCall("SetCursorPos", uintptr(saved.X), uintptr(saved.Y))
				}
			}()
			trayCall("SetCursorPos", uintptr(point.X), uintptr(point.Y))
			// TrackPopupMenu uses the real cursor position. Before clicking,
			// prove the point still belongs to this exact fixture process.
			target := trayCall("WindowFromPoint", uintptr(uint32(point.X))|uintptr(uint32(point.Y))<<32)
			var targetPID uint32
			trayCall("GetWindowThreadProcessId", target, uintptr(unsafe.Pointer(&targetPID)))
			if int(targetPID) != cmd.Process.Pid || target != popup {
				t.Fatalf("mouse target differs: target=%x pid=%d popup=%x fixture=%d rect=%+v point=%+v", target, targetPID, popup, cmd.Process.Pid, rect, point)
			}
			trayCall("mouse_event", 0x2, 0, 0, 0, 0)
			trayCall("mouse_event", 0x4, 0, 0, 0, 0)
			if s := status(model.AllowSleep); s.NativeRequestActive || s.Deadline != nil {
				t.Fatal("Allow sleep retained the timer or native request", s)
			}
			if console != nil && console.status(t, "").Mode != model.AllowSleep {
				t.Fatal("console disagrees with native menu")
			}
			_, menu = open(0x205)
			quit := trayMenuItemID(menu, "Quit SnooZap")
			if quit == ^uintptr(0) || quit == 0xffffffff {
				t.Fatal("native Quit command not found")
			}
			trayCall("PostMessageW", hwnd, 0x1f, 0, 0)
			trayCall("PostMessageW", hwnd, 0x111, quit, 0)
			if err := cmd.Wait(); err != nil {
				t.Fatal("native Quit failed", err)
			}
			if console != nil {
				console.stdin.Close()
				if err := console.cmd.Wait(); err != nil {
					t.Fatal("tray Quit did not stop the attached owner", err)
				}
				console.cancel()
			}
			owner, err := native.AcquireOwner()
			if err != nil {
				t.Fatal("native tray Quit retained engine ownership", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTrayControllerBatteryCutoffAndTelemetryFailure(t *testing.T) {
	var unavailable atomic.Bool
	percent := 10
	events := make(chan model.LifecycleEvent, 1)
	service := engine.New(context.Background(), &trayPower{}, nil, "", engine.Options{
		Events: events,
		ReadPower: func(context.Context) (model.Battery, error) {
			if unavailable.Load() {
				return model.Battery{}, errors.New("fixture telemetry failure")
			}
			return model.Battery{Known: true, Source: "battery", Present: true, Percent: &percent}, nil
		},
	})
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.Battery.Blocked })
	if s.Snapshot.Mode != model.Awake || s.Snapshot.NativeRequestActive || desktop.Describe(s).Title != "Sleep prevention paused" {
		t.Fatal("battery guard did not reach the tray truthfully", s)
	}
	unavailable.Store(true)
	events <- model.PowerChanged
	s = waitTray(t, updates, func(s desktop.State) bool { return !s.Snapshot.Battery.Known && s.Snapshot.Battery.Blocked })
	if s.Snapshot.NativeRequestAccepted || desktop.Describe(s).Icon != "paused" {
		t.Fatal("telemetry failure overclaimed protection", s)
	}
}
