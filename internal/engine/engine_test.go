package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
	powerprobe "github.com/omry/snoozap/internal/probe/power"
	processprobe "github.com/omry/snoozap/internal/probe/process"
)

type fakeClock struct {
	mu      sync.Mutex
	elapsed time.Duration
	wall    time.Time
	timers  []*fakeTimer

	sleep time.Duration
}
type fakeTimer struct {
	clock   *fakeClock
	channel chan time.Time
	due     time.Duration
	stopped bool
}

func newClock() *fakeClock                  { return &fakeClock{wall: time.Unix(1700000000, 0)} }
func (c *fakeClock) Now() time.Time         { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *fakeClock) Elapsed() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.elapsed }
func (c *fakeClock) NewTimer(d time.Duration) model.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, channel: make(chan time.Time, 1), due: c.elapsed + d}
	c.timers = append(c.timers, t)
	return t
}
func (t *fakeTimer) C() <-chan time.Time { return t.channel }
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	old := t.stopped
	t.stopped = true
	return !old
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elapsed += d
	c.wall = c.wall.Add(d)
	for _, t := range c.timers {
		if !t.stopped && t.due <= c.elapsed {
			t.stopped = true
			t.channel <- c.wall
		}
	}
}
func (c *fakeClock) shiftWall(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
}

type fakeBackend struct {
	mu     sync.Mutex
	calls  []bool
	active bool
	fail   int
	closed bool
}

func (p *fakeBackend) SetRequired(ctx context.Context, active bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.calls = append(p.calls, active)
	if p.fail > 0 {
		p.fail--
		return errors.New("denied")
	}
	p.active = active
	return nil
}
func (p *fakeBackend) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.active = false
	return nil
}
func (p *fakeBackend) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }

type probeFunc func(context.Context) model.Observation

func (f probeFunc) Check(ctx context.Context) model.Observation { return f(ctx) }

type rig struct {
	engine     *Engine
	clock      *fakeClock
	backend    *fakeBackend
	mu         sync.Mutex
	battery    model.Battery
	batteryErr error
	saveErr    error
	saves      int
	events     chan model.LifecycleEvent
}

func newRig(t *testing.T, cfg config.Config, resolve func(config.Probe) model.Probe) *rig {
	t.Helper()
	r := &rig{clock: newClock(), backend: &fakeBackend{}, battery: model.Battery{Source: "ac", Known: true}, events: make(chan model.LifecycleEvent, 16)}
	opts := Options{Configuration: cfg, Resolve: resolve, Events: r.events, ReadPower: func(context.Context) (model.Battery, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		b := r.battery
		b.Percent = copyInt(b.Percent)
		return b, r.batteryErr
	}, Save: func(config.Config) error { r.mu.Lock(); defer r.mu.Unlock(); r.saves++; return r.saveErr }}
	r.engine = New(context.Background(), r.backend, r.clock, "", opts)
	t.Cleanup(func() { _ = r.engine.Close(context.Background()) })
	r.status(t)
	return r
}
func (r *rig) status(t *testing.T) model.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := r.engine.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (r *rig) command(t *testing.T, c model.Command) model.Snapshot {
	t.Helper()
	s, err := r.engine.Execute(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (r *rig) until(t *testing.T, predicate func(model.Snapshot) bool) model.Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var s model.Snapshot
	for time.Now().Before(deadline) {
		s = r.status(t)
		if predicate(s) {
			return s
		}
		runtime.Gosched()
	}
	t.Fatalf("condition not reached: %+v", s)
	return s
}
func (r *rig) batteryAt(source string, percent int, known, present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.battery = model.Battery{Source: source, Known: known, Present: present}
	if percent >= 0 {
		r.battery.Percent = &percent
	}
}
func parse(t *testing.T, text string) config.Config {
	t.Helper()
	c, err := config.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func processConfig(t *testing.T, policy string) config.Config {
	return parse(t, fmt.Sprintf(`{"schema_version":1,"probes":{"app":{"type":"process","name":"work.exe","interval_ms":1000,"timeout_ms":100,"on_error":%q,"grace_seconds":2}},"rules":[{"id":"work","when":{"probe":"app"}}]}`, policy))
}
func waitCheck(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not start")
	}
}
func TestManualTimerReplacesClearsAndIgnoresCalendarChange(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	s := r.command(t, model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Hour})
	if !s.NativeRequestAccepted || s.Deadline == nil {
		t.Fatal(s)
	}
	r.clock.shiftWall(24 * time.Hour)
	r.clock.advance(30 * time.Minute)
	r.until(t, func(s model.Snapshot) bool { return s.RemainingMS == int64((30*time.Minute)/time.Millisecond) })
	r.command(t, model.Command{Operation: "mode", Mode: model.AllowSleep, Duration: time.Hour})
	r.clock.advance(30 * time.Minute)
	r.until(t, func(s model.Snapshot) bool {
		return s.RemainingMS == int64((30*time.Minute)/time.Millisecond) && s.Mode == model.AllowSleep
	})
	r.clock.advance(30 * time.Minute)
	r.until(t, func(s model.Snapshot) bool { return s.Mode == model.Auto && s.Deadline == nil && !s.DesiredHold })
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Minute})
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake})
	r.clock.advance(time.Hour)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted && s.Deadline == nil })
	if _, err := r.engine.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Auto, Duration: time.Second}); err == nil {
		t.Fatal("timed Auto accepted")
	}
}
func TestBatteryHysteresisTelemetryAndManualIndependence(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.command(t, model.Command{Operation: "monitor", Monitoring: false})
	if s := r.command(t, model.Command{Operation: "mode", Mode: model.Awake}); !s.NativeRequestAccepted || s.Monitoring {
		t.Fatal("manual hold depended on monitoring")
	}
	r.batteryAt("battery", 20, true, true)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return s.Battery.Blocked && !s.DesiredHold && s.Mode == model.Awake })
	r.batteryAt("battery", 24, true, true)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool {
		return s.Battery.Percent != nil && *s.Battery.Percent == 24 && s.Battery.Blocked
	})
	r.batteryAt("battery", 25, true, true)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return !s.Battery.Blocked && s.NativeRequestAccepted })
	r.mu.Lock()
	r.batteryErr = errors.New("unavailable")
	r.mu.Unlock()
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return s.Battery.Blocked && !s.NativeRequestActive })
	r.mu.Lock()
	r.batteryErr = nil
	r.mu.Unlock()
	r.batteryAt("ac", -1, true, false)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool {
		return !s.Battery.Blocked && s.NativeRequestAccepted && s.Battery.Percent == nil
	})
	cfg := config.Empty()
	cfg.Settings.Battery.ACOnly = true
	r.command(t, model.Command{Operation: "config", Configuration: &cfg})
	r.batteryAt("battery", 100, true, true)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return s.Battery.Blocked && !s.NativeRequestActive })
	r.batteryAt("ac", 5, true, true)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted && !s.Battery.Blocked })
}
func TestApplicationActivityAndFailureGraceDoNotRefresh(t *testing.T) {
	var mu sync.Mutex
	observation := model.Observation{State: model.Active}
	called := make(chan struct{}, 20)
	adapter := probeFunc(func(context.Context) model.Observation {
		mu.Lock()
		defer mu.Unlock()
		called <- struct{}{}
		return observation
	})
	r := newRig(t, processConfig(t, "release_after"), func(config.Probe) model.Probe { return adapter })
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted && s.Activity == model.Active })
	mu.Lock()
	observation = model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Unknown && s.DesiredHold && s.Rules[0].Retained
	})
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool { return s.Probes[0].ObservedAt.Equal(r.clock.Now()) })
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return !s.DesiredHold && !s.NativeRequestActive && s.Activity == model.Unknown
	})
	mu.Lock()
	observation = model.Observation{State: model.Inactive}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool { return s.Activity == model.Inactive && !s.DesiredHold })
	mu.Lock()
	observation = model.Observation{State: model.Active}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted })
}

func TestExpiredReleaseAfterReasonIdentifiesRuleAndProbe(t *testing.T) {
	var mu sync.Mutex
	observation := model.Observation{State: model.Active}
	called := make(chan struct{}, 20)
	adapter := probeFunc(func(context.Context) model.Observation {
		mu.Lock()
		defer mu.Unlock()
		called <- struct{}{}
		return observation
	})
	r := newRig(t, processConfig(t, "release_after"), func(config.Probe) model.Probe { return adapter })
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted && s.Activity == model.Active })

	mu.Lock()
	observation = model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Unknown && s.DesiredHold && strings.Contains(s.Reason, "retained:work")
	})
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.clock.advance(time.Second)
	waitCheck(t, called)
	s := r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Unknown && !s.DesiredHold && !s.NativeRequestActive && strings.Contains(s.Reason, "release_after expired: rule:work probe:app")
	})
	if again := r.status(t); again.Reason != s.Reason || again.DesiredHold || again.NativeRequestActive {
		t.Fatalf("expired policy explanation did not persist: first=%+v again=%+v", s, again)
	}

	mu.Lock()
	observation = model.Observation{State: model.Inactive}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	s = r.until(t, func(s model.Snapshot) bool { return s.Activity == model.Inactive && !s.DesiredHold })
	if s.Reason != "Automatic rules inactive" {
		t.Fatalf("recovery retained stale expiry explanation: %+v", s)
	}
}

func TestExpiredReleaseReasonDoesNotMaskIndependentHold(t *testing.T) {
	var mu sync.Mutex
	observations := map[string]model.State{"work.exe": model.Active, "other.exe": model.Active}
	called := make(chan struct{}, 20)
	cfg := parse(t, `{"schema_version":1,"probes":{"work":{"type":"process","name":"work.exe","interval_ms":1000,"timeout_ms":100,"on_error":"release_after","grace_seconds":2},"other":{"type":"process","name":"other.exe","interval_ms":1000,"timeout_ms":100,"on_error":"retain","grace_seconds":2}},"rules":[{"id":"work","when":{"probe":"work"}},{"id":"other","when":{"probe":"other"}}]}`)
	resolve := func(spec config.Probe) model.Probe {
		return probeFunc(func(context.Context) model.Observation {
			mu.Lock()
			state := observations[spec.Name]
			mu.Unlock()
			called <- struct{}{}
			return model.Observation{State: state}
		})
	}
	r := newRig(t, cfg, resolve)
	waitCheck(t, called)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return len(s.Rules) == 2 && s.Rules[0].State == model.Active && s.Rules[1].State == model.Active && s.NativeRequestAccepted
	})

	mu.Lock()
	observations["work.exe"] = model.Unknown
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return len(s.Rules) == 2 && s.Rules[0].State == model.Unknown && s.Rules[1].State == model.Active && s.DesiredHold
	})
	r.clock.advance(2 * time.Second)
	waitCheck(t, called)
	waitCheck(t, called)
	s := r.until(t, func(s model.Snapshot) bool {
		return len(s.Rules) == 2 && s.Rules[0].State == model.Unknown && s.Rules[1].State == model.Active && s.DesiredHold
	})
	if s.Reason != "rule:other" || strings.Contains(s.Reason, "release_after expired") {
		t.Fatalf("independent active rule did not retain precedence: %+v", s)
	}
}

func TestMonitoringOffReasonPreservesUnknownAndOverrides(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	s := r.command(t, model.Command{Operation: "monitor", Monitoring: false})
	if s.Activity != model.Unknown || s.DesiredHold || s.NativeRequestActive || s.Reason != "Monitoring off" {
		t.Fatalf("monitoring-off explanation changed state or precedence: %+v", s)
	}
	r.batteryAt("battery", 20, true, true)
	r.events <- model.PowerChanged
	s = r.until(t, func(s model.Snapshot) bool { return s.Battery.Blocked })
	if s.Reason != "Battery floor protection" || s.DesiredHold || s.NativeRequestActive {
		t.Fatalf("battery precedence changed while monitoring is off: %+v", s)
	}
	r.batteryAt("ac", -1, true, false)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return !s.Battery.Blocked })
	s = r.command(t, model.Command{Operation: "mode", Mode: model.Awake})
	if s.Reason != "Manual Stay awake" || !s.DesiredHold || !s.NativeRequestActive {
		t.Fatalf("manual precedence changed while monitoring is off: %+v", s)
	}
	s = r.command(t, model.Command{Operation: "mode", Mode: model.AllowSleep})
	if !strings.Contains(s.Reason, "does not issue Sleep") || s.DesiredHold || s.NativeRequestActive {
		t.Fatalf("Allow sleep precedence changed while monitoring is off: %+v", s)
	}
	r.events <- model.Suspend
	s = r.until(t, func(s model.Snapshot) bool { return s.Reason == "System suspended; protection released" })
	if s.DesiredHold || s.NativeRequestActive {
		t.Fatalf("suspend precedence changed while monitoring is off: %+v", s)
	}
	r.events <- model.Resume
	r.until(t, func(s model.Snapshot) bool { return strings.Contains(s.Reason, "does not issue Sleep") })
}

func TestReasonDistinguishesInactiveConfiguredRules(t *testing.T) {
	called := make(chan struct{}, 4)
	adapter := probeFunc(func(context.Context) model.Observation {
		called <- struct{}{}
		return model.Observation{State: model.Inactive}
	})
	r := newRig(t, processConfig(t, "retain"), func(config.Probe) model.Probe { return adapter })
	waitCheck(t, called)
	s := r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Inactive && len(s.Rules) == 1 && s.Rules[0].State == model.Inactive
	})
	if s.DesiredHold || s.NativeRequestActive || s.Reason != "Automatic rules inactive" {
		t.Fatalf("inactive configured rule was misreported: %+v", s)
	}

	empty := newRig(t, config.Empty(), nil)
	if s := empty.status(t); s.Reason != "No automatic rules are configured" {
		t.Fatalf("empty configuration changed reason: %+v", s)
	}

	disabledConfig := processConfig(t, "retain")
	disabledConfig.Rules[0].Enabled = false
	disabled := newRig(t, disabledConfig, nil)
	if s := disabled.status(t); len(s.Rules) != 0 || s.Reason != "No automatic rules are configured" {
		t.Fatalf("disabled rule changed empty-rule semantics: %+v", s)
	}

	unknownCalled := make(chan struct{}, 4)
	unknown := newRig(t, processConfig(t, "retain"), func(config.Probe) model.Probe {
		return probeFunc(func(context.Context) model.Observation {
			unknownCalled <- struct{}{}
			return model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
		})
	})
	waitCheck(t, unknownCalled)
	s = unknown.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Unknown && len(s.Rules) == 1
	})
	if s.Reason != "Not confirmed — no automatic hold" || s.DesiredHold || s.NativeRequestActive {
		t.Fatalf("unknown configured rule changed precedence: %+v", s)
	}

	r.batteryAt("battery", 20, true, true)
	r.events <- model.PowerChanged
	s = r.until(t, func(s model.Snapshot) bool { return s.Battery.Blocked })
	if s.Reason != "Battery floor protection" || s.DesiredHold || s.NativeRequestActive {
		t.Fatalf("battery precedence changed: %+v", s)
	}
	r.batteryAt("ac", -1, true, false)
	r.events <- model.PowerChanged
	r.until(t, func(s model.Snapshot) bool { return !s.Battery.Blocked })
	s = r.command(t, model.Command{Operation: "mode", Mode: model.Awake})
	if s.Reason != "Manual Stay awake" || !s.DesiredHold || !s.NativeRequestActive {
		t.Fatalf("manual override precedence changed: %+v", s)
	}
	s = r.command(t, model.Command{Operation: "mode", Mode: model.AllowSleep})
	if !strings.Contains(s.Reason, "does not issue Sleep") || s.DesiredHold || s.NativeRequestActive {
		t.Fatalf("allow-sleep override precedence changed: %+v", s)
	}
}
func TestInvalidOrUnpersistableEditPreservesGenerationAndTimer(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Hour})
	before := r.status(t)
	bad := config.Empty()
	bad.SchemaVersion = 2
	if _, err := r.engine.Execute(context.Background(), model.Command{Operation: "config", Configuration: &bad}); err == nil {
		t.Fatal("invalid edit accepted")
	}
	r.mu.Lock()
	r.saveErr = errors.New("disk full")
	r.mu.Unlock()
	valid := processConfig(t, "retain")
	if _, err := r.engine.Execute(context.Background(), model.Command{Operation: "config", Configuration: &valid}); err == nil {
		t.Fatal("failed persistence activated candidate")
	}
	after := r.status(t)
	if after.Sequence != before.Sequence || !after.Deadline.Equal(*before.Deadline) || len(after.Probes) != 0 || !after.NativeRequestAccepted {
		t.Fatalf("failed edit changed live state: %+v", after)
	}
	r.mu.Lock()
	r.saveErr = nil
	r.mu.Unlock()
	r.command(t, model.Command{Operation: "monitor", Monitoring: false})
	if s := r.status(t); s.Monitoring || s.Deadline == nil || !s.NativeRequestAccepted {
		t.Fatal("monitoring edit replaced manual timer")
	}
}

func TestOversizedPersistedCandidatePreservesManualTimer(t *testing.T) {
	clock := newClock()
	power := &fakeBackend{}
	writes := 0
	store := config.Store{Replace: func([]byte) error {
		writes++
		return nil
	}}
	options := Options{
		Configuration: config.Empty(),
		ReadPower: func(context.Context) (model.Battery, error) {
			return model.Battery{Source: "ac", Known: true}, nil
		},
		Save: store.Save,
	}
	e := New(context.Background(), power, clock, "", options)
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	before, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Hour})
	if err != nil || before.Deadline == nil || !before.NativeRequestAccepted {
		t.Fatalf("manual timer setup failed: %+v %v", before, err)
	}
	candidate := config.Empty()
	for n := range 256 {
		candidate.Probes[fmt.Sprintf("p%d", n)] = config.Probe{
			Type:             "process",
			Enabled:          true,
			OnStartupUnknown: "wait",
			OnError:          "retain",
			GraceSeconds:     60,
			IntervalMS:       5000,
			TimeoutMS:        2000,
			Name:             strings.Repeat("x", 4000),
		}
	}
	if _, err := e.Execute(context.Background(), model.Command{Operation: "config", Configuration: &candidate}); err == nil {
		t.Fatal("oversized candidate was reported as persisted")
	}
	after, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 || len(after.Probes) != 0 || after.Deadline == nil || !after.Deadline.Equal(*before.Deadline) || after.Mode != model.Awake || !after.NativeRequestAccepted {
		t.Fatalf("failed persistence changed live state: writes=%d before=%+v after=%+v", writes, before, after)
	}
}
func TestStaleResultAfterDisableCannotAcquireAndWorkersStayBounded(t *testing.T) {
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	adapter := probeFunc(func(context.Context) model.Observation {
		started <- struct{}{}
		<-release
		return model.Observation{State: model.Active}
	})
	cfg := processConfig(t, "retain")
	r := newRig(t, cfg, func(config.Probe) model.Probe { return adapter })
	waitCheck(t, started)
	r.command(t, model.Command{Operation: "monitor", Monitoring: false})
	once.Do(func() { close(release) })
	for range 20 {
		s := r.status(t)
		if s.DesiredHold || s.NativeRequestActive || s.Monitoring {
			t.Fatal("obsolete result acquired a hold")
		}
	}
}
func TestTimedModeExpiresBeforeResumeAcquisition(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Minute})
	r.events <- model.Suspend
	r.until(t, func(s model.Snapshot) bool { return !s.NativeRequestActive })
	calls := r.backend.count()
	r.clock.advance(2 * time.Minute)
	r.events <- model.Resume
	r.until(t, func(s model.Snapshot) bool { return s.Mode == model.Auto && !s.NativeRequestAccepted })
	if r.backend.count() != calls {
		t.Fatal("expired manual hold reacquired during resume")
	}
}
func TestNativeFailureRetriesUseBackoffAndReleaseCancels(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.backend.mu.Lock()
	r.backend.fail = 2
	r.backend.mu.Unlock()
	if s, err := r.engine.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake}); err == nil || s.NativeRequestAccepted {
		t.Fatal("failed native request claimed protection")
	}
	calls := r.backend.count()
	r.clock.advance(500 * time.Millisecond)
	for range 20 {
		r.status(t)
	}
	if r.backend.count() != calls {
		t.Fatal("native failure retried before backoff")
	}
	r.clock.advance(500 * time.Millisecond)
	r.until(t, func(model.Snapshot) bool { return r.backend.count() == calls+1 })
	r.command(t, model.Command{Operation: "mode", Mode: model.Auto})
	r.clock.advance(time.Minute)
	for range 20 {
		if r.status(t).NativeRequestAccepted {
			t.Fatal("release failed to cancel native retry")
		}
	}
	if r.backend.count() != calls+1 {
		t.Fatal("cancelled acquisition retried")
	}
}
func TestTimeoutDoesNotLeaveActiveObservationFresh(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	adapter := probeFunc(func(context.Context) model.Observation {
		started <- struct{}{}
		<-release
		return model.Observation{State: model.Active}
	})
	r := newRig(t, processConfig(t, "retain"), func(config.Probe) model.Probe { return adapter })
	waitCheck(t, started)
	r.clock.advance(100 * time.Millisecond)
	r.until(t, func(s model.Snapshot) bool {
		return len(s.Probes) == 1 && s.Probes[0].ErrorCode == "timeout" && !s.DesiredHold
	})
	// This adapter ignores cancellation beyond the bounded handoff period.
	r.clock.advance(cancellationGrace)
	for range 20 {
		r.status(t)
	}
	once.Do(func() { close(release) })
	for range 20 {
		if r.status(t).DesiredHold {
			t.Fatal("late timed-out result acquired protection")
		}
	}
	r.clock.advance(5 * time.Second)
	for range 20 {
		r.status(t)
	}
	select {
	case <-started:
		t.Fatal("wedged adapter rescheduled without reset")
	default:
	}
}

func TestCooperativeTimeoutRecoversEvenWhenResultDeliveryIsDelayed(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed=%t", delayed), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newClock()
			spec := processConfig(t, "retain").Probes["app"]
			started := make(chan struct{}, 2)
			adapter := probeFunc(func(ctx context.Context) model.Observation {
				started <- struct{}{}
				<-ctx.Done()
				return model.Observation{State: model.Unknown, ErrorCode: "timeout"}
			})
			l := &loop{ctx: ctx, clock: clock, options: Options{Resolve: func(config.Probe) model.Probe { return adapter }}, probes: map[string]*probeState{"app": {spec: spec}}, jobs: map[string]*poll{}, results: make(chan result, 8)}
			l.snapshot.Monitoring = true
			l.poll(0)
			waitCheck(t, started)
			clock.advance(100 * time.Millisecond)
			l.poll(clock.Elapsed())
			var completed result
			select {
			case completed = <-l.results:
			case <-time.After(time.Second):
				t.Fatal("cooperative adapter did not return after cancellation")
			}
			if delayed {
				clock.advance(time.Second)
				l.poll(clock.Elapsed())
			}
			l.receive(completed, clock.Elapsed())
			if l.probes["app"].state != model.Unknown {
				t.Fatal("expired observation became fresh")
			}
			clock.advance(time.Second)
			l.poll(clock.Elapsed())
			waitCheck(t, started)
			for _, job := range l.jobs {
				job.cancel()
			}
		})
	}
}
func TestSchedulerConcurrencyLimitAcrossGenerations(t *testing.T) {
	cfg := config.Empty()
	for n := range 20 {
		spec := processConfig(t, "retain").Probes["app"]
		cfg.Probes[fmt.Sprintf("p%d", n)] = spec
	}
	started := make(chan struct{}, 40)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	adapter := probeFunc(func(context.Context) model.Observation {
		started <- struct{}{}
		<-release
		return model.Observation{State: model.Active}
	})
	r := newRig(t, cfg, func(config.Probe) model.Probe { return adapter })
	for range 8 {
		waitCheck(t, started)
	}
	for range 20 {
		r.status(t)
	}
	select {
	case <-started:
		t.Fatal("more than eight simultaneous checks")
	default:
	}
	r.command(t, model.Command{Operation: "monitor", Monitoring: false})
	r.command(t, model.Command{Operation: "monitor", Monitoring: true})
	for range 20 {
		r.status(t)
	}
	select {
	case <-started:
		t.Fatal("generation edit abandoned worker accounting")
	default:
	}
	once.Do(func() { close(release) })
}

type fakeProcessReader struct {
	observation model.Observation
	name, path  string
}

func (p *fakeProcessReader) Match(_ context.Context, name, path string) model.Observation {
	p.name = name
	p.path = path
	return p.observation
}

type fakePowerReader struct {
	battery model.Battery
	err     error
}

func (p fakePowerReader) ReadPower(context.Context) (model.Battery, error) { return p.battery, p.err }
func TestProbeAdaptersPreserveSelectorAndUnknownTelemetry(t *testing.T) {
	reader := &fakeProcessReader{observation: model.Observation{State: model.Inactive}}
	p := processprobe.Probe{Reader: reader, Executable: `C:\Work\render.exe`}
	if p.Check(context.Background()).State != model.Inactive || reader.path != p.Executable || reader.name != "" {
		t.Fatal("process selector changed")
	}
	for _, test := range []struct {
		reader fakePowerReader
		source string
		want   model.State
	}{{fakePowerReader{battery: model.Battery{Source: "ac", Known: true}}, "ac", model.Active}, {fakePowerReader{battery: model.Battery{Source: "battery", Known: true}}, "ac", model.Inactive}, {fakePowerReader{err: errors.New("failure")}, "battery", model.Unknown}, {fakePowerReader{battery: model.Battery{Source: "unknown", Known: true}}, "ac", model.Unknown}} {
		if got := (powerprobe.Probe{Reader: test.reader, Source: test.source}).Check(context.Background()); got.State != test.want {
			t.Fatal(got)
		}
	}
}

func TestResumeClearFailureCannotReuseOldAcceptance(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake})
	r.backend.mu.Lock()
	r.backend.fail = 1
	r.backend.mu.Unlock()
	calls := r.backend.count()
	r.events <- model.Resume
	s := r.until(t, func(s model.Snapshot) bool { return r.backend.count() > calls })
	if s.NativeRequestAccepted || s.Hold.OSAccepted || s.NativeError == "" {
		t.Fatal("old acceptance survived failed resume reconciliation:", s)
	}
	r.clock.advance(time.Second)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted && s.NativeError == "" })
}
func TestStartupUnknownDoesNotInventEarlyFailureEpisode(t *testing.T) {
	p := &probeState{spec: config.Probe{Enabled: true, OnError: "release_after", GraceSeconds: 1, OnStartupUnknown: "hold"}, state: model.Unknown}
	probes := map[string]*probeState{"p": p}
	v := truth{state: model.Unknown, unknown: map[string]bool{"p": true}}
	if hold, _ := retainUnknown(v, true, probes, 2*time.Second); !hold {
		t.Fatal("grace started before first failure")
	}
	p.failing = true
	p.failureSince = 2 * time.Second
	if hold, _ := retainUnknown(v, true, probes, 3*time.Second); hold {
		t.Fatal("actual first failure did not start grace")
	}
}

func TestStartupReleaseAfterDoesNotReacquireWithinFailureEpisode(t *testing.T) {
	var mu sync.Mutex
	observation := model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	called := make(chan struct{}, 20)
	adapter := probeFunc(func(context.Context) model.Observation {
		mu.Lock()
		value := observation
		mu.Unlock()
		called <- struct{}{}
		return value
	})
	cfg := parse(t, `{"schema_version":1,"probes":{"app":{"type":"process","name":"work.exe","interval_ms":1000,"timeout_ms":100,"on_startup_unknown":"hold","on_error":"release_after","grace_seconds":2}},"rules":[{"id":"work","when":{"probe":"app"}}]}`)
	r := newRig(t, cfg, func(config.Probe) model.Probe { return adapter })
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return len(s.Rules) == 1 && s.Rules[0].Startup && s.DesiredHold
	})

	r.clock.advance(2 * time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return len(s.Rules) == 1 && !s.DesiredHold && !s.NativeRequestActive && !s.Rules[0].Startup
	})
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return len(s.Rules) == 1 && !s.DesiredHold && !s.NativeRequestActive && !s.Rules[0].Startup
	})

	mu.Lock()
	observation = model.Observation{State: model.Inactive}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Inactive && !s.DesiredHold && !s.Rules[0].Startup
	})

	mu.Lock()
	observation = model.Observation{State: model.Unknown, ErrorCode: "unavailable"}
	mu.Unlock()
	r.clock.advance(time.Second)
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Unknown && !s.DesiredHold && !s.Rules[0].Startup
	})

	r.command(t, model.Command{Operation: "monitor", Monitoring: false})
	r.command(t, model.Command{Operation: "monitor", Monitoring: true})
	waitCheck(t, called)
	r.until(t, func(s model.Snapshot) bool {
		return s.Activity == model.Unknown && s.DesiredHold && s.Rules[0].Startup
	})
}

func (c *fakeClock) SleepTime() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sleep, nil
}
func (c *fakeClock) suspendFor(d time.Duration) {
	c.mu.Lock()
	c.sleep += d
	c.mu.Unlock()
	c.advance(d)
}
func TestOrdinaryDelayDoesNotRenewSuccessfulPowerRequest(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake})
	calls := r.backend.count()
	r.clock.advance(time.Hour)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted })
	for range 20 {
		r.status(t)
	}
	if r.backend.count() != calls {
		t.Fatal("ordinary scheduling delay renewed a successful request")
	}
}
func TestMissedResumeExpiresTimerAndReacquiresOnlyUnexpiredHold(t *testing.T) {
	r := newRig(t, config.Empty(), nil)
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Minute})
	r.clock.suspendFor(2 * time.Minute)
	r.until(t, func(s model.Snapshot) bool { return s.Mode == model.Auto && !s.NativeRequestActive })
	r.command(t, model.Command{Operation: "mode", Mode: model.Awake})
	calls := r.backend.count()
	r.clock.suspendFor(time.Minute)
	r.until(t, func(s model.Snapshot) bool { return s.NativeRequestAccepted && r.backend.count() == calls+2 })
}

func TestInvalidStartupIsNotOverwrittenByMonitoringToggle(t *testing.T) {
	writes := 0
	options := Options{ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }, Save: func(config.Config) error { writes++; return nil }}
	e := New(context.Background(), &fakeBackend{}, nil, "Startup configuration rejected", options)
	defer e.Close(context.Background())
	if _, err := e.Execute(context.Background(), model.Command{Operation: "monitor", Monitoring: false}); err == nil || writes != 0 {
		t.Fatal("monitor toggle overwrote invalid startup file")
	}
	if s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake}); err != nil || !s.NativeRequestAccepted {
		t.Fatal("invalid file prevented direct manual control")
	}
	cfg := config.Empty()
	if s, err := e.Execute(context.Background(), model.Command{Operation: "config", Configuration: &cfg}); err != nil || s.ConfigurationError != "" || writes != 1 {
		t.Fatal("explicit repair did not replace invalid startup state")
	}
}
func TestSnapshotNestedValuesCannotMutateOwner(t *testing.T) {
	adapter := probeFunc(func(context.Context) model.Observation { return model.Observation{State: model.Active} })
	r := newRig(t, processConfig(t, "retain"), func(config.Probe) model.Probe { return adapter })
	r.batteryAt("battery", 80, true, true)
	r.events <- model.PowerChanged
	s := r.until(t, func(s model.Snapshot) bool {
		return s.NativeRequestAccepted && len(s.Probes) == 1 && s.Probes[0].ObservedAt != nil && s.Battery.Percent != nil
	})
	s.Hold.Reasons[0] = "invented"
	s.Probes[0].ID = "changed"
	*s.Probes[0].ObservedAt = time.Time{}
	s.Rules[0].State = model.Inactive
	*s.Battery.Percent = 1
	after := r.status(t)
	if after.Hold.Reasons[0] == "invented" || after.Probes[0].ID == "changed" || after.Probes[0].ObservedAt.IsZero() || after.Rules[0].State != model.Active || *after.Battery.Percent != 80 {
		t.Fatal("caller modified engine state through snapshot")
	}
}
