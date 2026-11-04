package engine

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/history"
	"github.com/omry/snoozap/internal/model"
)

type Options struct {
	Configuration config.Config
	Resolve       func(config.Probe) model.Probe
	ReadPower     func(context.Context) (model.Battery, error)
	Save          func(config.Config) error
	Events        <-chan model.LifecycleEvent
}
type response struct {
	snapshot      model.Snapshot
	history       history.Snapshot
	configuration config.Config
	err           error
}
type request struct {
	ctx   context.Context
	cmd   model.Command
	reply chan response
}
type Engine struct {
	requests chan request
	done     chan struct{}
	closeErr error
}
type loop struct {
	history                   history.Store
	ctx                       context.Context
	options                   Options
	power                     model.PowerBackend
	clock                     model.SchedulerClock
	cfg                       config.Config
	snapshot                  model.Snapshot
	probes                    map[string]*probeState
	jobs                      map[string]*poll
	results                   chan result
	holds                     map[string]bool
	startup                   map[string]bool
	generation, epoch         uint64
	cursor                    int
	deadline                  time.Duration
	deadlineSet               bool
	batteryLatched, suspended bool
	refreshRequired           bool
	retryAt                   time.Duration
	retryStep                 int
	retryDesired              bool
}

func New(ctx context.Context, power model.PowerBackend, clock model.Clock, configError string, opts ...Options) *Engine {
	if clock == nil {
		clock = realClock{}
	}
	var options Options
	if len(opts) > 0 {
		options = opts[0]
	}
	if options.Configuration.SchemaVersion == 0 {
		options.Configuration = config.Empty()
	}
	if err := config.Validate(options.Configuration); err != nil {
		options.Configuration = config.Empty()
		configError = "Invalid startup configuration"
	}
	options.Configuration = config.Clone(options.Configuration)
	timing, ok := clock.(model.SchedulerClock)
	if !ok {
		timing = schedulerClock{Clock: clock, origin: time.Now()}
	}
	e := &Engine{requests: make(chan request), done: make(chan struct{})}
	go e.run(ctx, power, timing, configError, options)
	return e
}
func (e *Engine) Snapshot(ctx context.Context) (model.Snapshot, error) {
	return e.Execute(ctx, model.Command{Operation: "status"})
}
func (e *Engine) Execute(ctx context.Context, cmd model.Command) (model.Snapshot, error) {
	r := e.call(ctx, cmd)
	return r.snapshot, r.err
}
func (e *Engine) History(ctx context.Context) (history.Snapshot, error) {
	r := e.call(ctx, model.Command{Operation: "history"})
	return r.history, r.err
}
func (e *Engine) ClearHistory(ctx context.Context) error {
	return e.call(ctx, model.Command{Operation: "history-clear"}).err
}
func (e *Engine) ExportConfig(ctx context.Context) ([]byte, error) {
	r := e.call(ctx, model.Command{Operation: "config-export"})
	if r.err != nil {
		return nil, r.err
	}
	return config.Export(r.configuration)
}
func (e *Engine) call(ctx context.Context, cmd model.Command) response {
	r := request{ctx: ctx, cmd: cmd, reply: make(chan response, 1)}
	if err := ctx.Err(); err != nil {
		return response{err: err}
	}
	select {
	case <-ctx.Done():
		return response{err: ctx.Err()}
	case <-e.done:
		return response{err: model.ErrStopped}
	case e.requests <- r:
	}
	select {
	case v := <-r.reply:
		return v
	case <-ctx.Done():
		return response{err: ctx.Err()}
	case <-e.done:
		select {
		case v := <-r.reply:
			return v
		default:
			return response{err: model.ErrStopped}
		}
	}
}
func (e *Engine) Close(ctx context.Context) error {
	_, err := e.Execute(ctx, model.Command{Operation: "stop"})
	if errors.Is(err, model.ErrStopped) {
		return e.closeErr
	}
	return err
}
func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
func (l *loop) view() model.Snapshot {
	s := l.snapshot
	s.Deadline = copyTime(s.Deadline)
	s.Battery.Percent = copyInt(s.Battery.Percent)
	s.Limitations = append([]string(nil), s.Limitations...)
	s.Hold.Reasons = append([]string{}, s.Hold.Reasons...)
	s.Rules = append([]model.RuleStatus{}, s.Rules...)
	s.Probes = append([]model.ProbeStatus{}, s.Probes...)
	for i := range s.Probes {
		s.Probes[i].ObservedAt = copyTime(s.Probes[i].ObservedAt)
		s.Probes[i].LastSuccessAt = copyTime(s.Probes[i].LastSuccessAt)
	}
	return s
}
func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
func (l *loop) publish(previous model.Snapshot) {
	if previous.Activity != l.snapshot.Activity {
		l.record("state", string(l.snapshot.Activity))
	}
	if previous.DesiredHold != l.snapshot.DesiredHold {
		result := "released"
		if l.snapshot.DesiredHold {
			result = "requested"
		}
		l.record("hold", result)
	}
	if previous.NativeRequestActive != l.snapshot.NativeRequestActive || previous.NativeRequestAccepted != l.snapshot.NativeRequestAccepted || previous.NativeError != l.snapshot.NativeError {
		result := "released"
		if l.snapshot.NativeRequestAccepted {
			result = "accepted"
		}
		if l.snapshot.NativeError != "" {
			result = "rejected"
		}
		l.record("native", result)
	}
	if previous.Battery.Blocked != l.snapshot.Battery.Blocked {
		result := "ok"
		if l.snapshot.Battery.Blocked {
			result = "blocked"
		}
		l.record("battery", result)
	}
	l.snapshot.Probes = []model.ProbeStatus{}
	ids := make([]string, 0, len(l.probes))
	for id := range l.probes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := l.probes[id]
		l.snapshot.Probes = append(l.snapshot.Probes, model.ProbeStatus{ID: id, State: p.state, ObservedAt: copyTime(p.observed), LastSuccessAt: copyTime(p.success), ErrorCode: p.code})
	}
	if !reflect.DeepEqual(previous, l.snapshot) {
		l.snapshot.Sequence++
		l.snapshot.UpdatedAt = l.clock.Now()
	}
}

func (l *loop) record(operation, result string) {
	l.history.Add(history.Record{Time: l.clock.Now(), Operation: operation, Result: result})
}

func (l *loop) recordControl(cmd model.Command, err error) {
	result := "ok"
	if err != nil {
		result = "rejected"
	}
	switch cmd.Operation {
	case "mode":
		if err == nil {
			result = string(cmd.Mode)
		}
	case "monitor":
		if err == nil {
			result = "off"
			if cmd.Monitoring {
				result = "on"
			}
		}
	case "config":
	default:
		return
	}
	l.record(cmd.Operation, result)
}
func (l *loop) activate(candidate config.Config, now time.Duration) {
	candidate = config.Clone(candidate)
	changed := map[string]bool{}
	for id, p := range l.probes {
		if next, ok := candidate.Probes[id]; !ok || !reflect.DeepEqual(next, p.spec) {
			changed[id] = true
		}
	}
	next := map[string]*probeState{}
	for id, spec := range candidate.Probes {
		if p := l.probes[id]; p != nil && !changed[id] {
			next[id] = p
			continue
		}
		changed[id] = true
		p := &probeState{spec: spec, state: model.Unknown, code: "not_observed", next: now}
		next[id] = p
	}
	oldRules := map[string]config.Rule{}
	for _, r := range l.cfg.Rules {
		oldRules[r.ID] = r
	}
	holds, startup := map[string]bool{}, map[string]bool{}
	for _, r := range candidate.Rules {
		unchanged := reflect.DeepEqual(oldRules[r.ID], r)
		for id := range changed {
			if references(r.When, id) {
				unchanged = false
			}
		}
		if unchanged {
			holds[r.ID] = l.holds[r.ID]
			startup[r.ID] = l.startup[r.ID]
		}
	}
	monitoringChanged := l.snapshot.Monitoring != candidate.Settings.Monitoring
	for _, job := range l.jobs {
		job.cancel()
	}
	l.generation++
	l.cfg = candidate
	l.probes = next
	l.holds = holds
	l.startup = startup
	l.snapshot.Monitoring = candidate.Settings.Monitoring
	if monitoringChanged {
		l.invalidate(now, true)
	}
}
func (l *loop) cleanup() error {
	for _, job := range l.jobs {
		job.cancel()
	}
	var err error
	if l.snapshot.NativeRequestActive {
		err = l.power.SetRequired(context.Background(), false)
	}
	closeErr := l.power.Close()
	if closeErr == nil {
		l.snapshot.NativeRequestActive = false
	}
	l.snapshot.NativeRequestAccepted = false
	l.snapshot.DesiredHold = false
	l.snapshot.Hold = model.Hold{Reasons: []string{}}
	l.snapshot.Mode = model.Auto
	l.snapshot.Deadline = nil
	l.snapshot.RemainingMS = 0
	l.snapshot.Reason = "Runtime stopped"
	l.snapshot.Sequence++
	l.snapshot.UpdatedAt = l.clock.Now()
	if err != nil || closeErr != nil {
		l.snapshot.NativeError = "Native request cleanup failed"
	}
	return errors.Join(err, closeErr)
}
func (l *loop) event(event model.LifecycleEvent, now time.Duration) {
	switch event {
	case model.Suspend:
		l.suspended = true
		l.invalidate(now, false)
	case model.Resume:
		l.suspended = false
		l.invalidate(now, false)
		// An old acceptance cannot prove that protection survived resume.
		l.refreshRequired = true
	}
	l.readBattery()
}
func (l *loop) execute(cmd model.Command, now time.Duration) error {
	switch cmd.Operation {
	case "status":
		return nil
	case "mode":
		if !cmd.Mode.Valid() || cmd.Duration < 0 || cmd.Mode == model.Auto && cmd.Duration != 0 || int64(cmd.Duration) > math.MaxInt64-int64(now) {
			return errors.New("invalid manual mode or duration")
		}
		l.snapshot.Mode = cmd.Mode
		l.deadlineSet = cmd.Duration > 0
		l.snapshot.Deadline = nil
		if l.deadlineSet {
			l.deadline = now + cmd.Duration
			wall := l.clock.Now().Add(cmd.Duration)
			l.snapshot.Deadline = &wall
		}
	case "monitor", "config":
		candidate := l.cfg
		if cmd.Operation == "monitor" {
			if l.snapshot.ConfigurationError != "" {
				return errors.New("repair startup configuration before saving monitoring settings")
			}
			candidate.Settings.Monitoring = cmd.Monitoring
		} else {
			if cmd.Configuration == nil {
				return errors.New("missing configuration")
			}
			candidate = *cmd.Configuration
		}
		if err := config.Validate(candidate); err != nil {
			return err
		}
		candidate = config.Clone(candidate)
		if l.options.Save == nil {
			return errors.New("configuration storage unavailable")
		}
		if err := l.options.Save(candidate); err != nil {
			return errors.New("configuration could not be persisted; prior settings remain active")
		}
		l.activate(candidate, now)
		l.snapshot.ConfigurationError = ""
	default:
		return errors.New("unknown runtime operation")
	}
	l.readBattery()
	l.poll(now)
	return l.reconcile(now)
}
func (e *Engine) run(ctx context.Context, power model.PowerBackend, clock model.SchedulerClock, configError string, options Options) {
	defer close(e.done)
	ownerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	l := &loop{ctx: ownerCtx, power: power, clock: clock, options: options, cfg: config.Empty(), probes: map[string]*probeState{}, jobs: map[string]*poll{}, results: make(chan result, 8), holds: map[string]bool{}, startup: map[string]bool{}}
	l.snapshot = model.Snapshot{SchemaVersion: 1, Sequence: 1, Mode: model.Auto, Monitoring: true, UpdatedAt: clock.Now(), ConfigurationError: configError, Activity: model.Unknown, Limitations: []string{"An accepted request does not guarantee protection from explicit Sleep or lid close.", "Windows Modern Standby and battery policies can limit SystemRequired requests.", "SnooZap does not request that the display stay on."}}
	previous := l.view()
	now := clock.Elapsed()
	l.activate(options.Configuration, now)
	l.readBattery()
	l.poll(now)
	_ = l.reconcile(now)
	l.publish(previous)
	timer := clock.NewTimer(100 * time.Millisecond)
	defer func() { timer.Stop() }()
	suspendClock, hasSuspendClock := clock.(model.SuspendClock)
	var lastSleep time.Duration
	if hasSuspendClock {
		var err error
		lastSleep, err = suspendClock.SleepTime()
		if err != nil {
			l.snapshot.Limitations = append(l.snapshot.Limitations, "Periodic resume detection unavailable; native notifications remain active.")
		}
	}
	events := options.Events
	for {
		previous := l.view()
		select {
		case <-ctx.Done():
			e.closeErr = l.cleanup()
			return
		case r := <-e.requests:
			if err := r.ctx.Err(); err != nil {
				r.reply <- response{err: err}
				continue
			}
			if r.cmd.Operation == "stop" {
				e.closeErr = l.cleanup()
				r.reply <- response{snapshot: l.view(), err: e.closeErr}
				return
			}
			switch r.cmd.Operation {
			case "history":
				r.reply <- response{snapshot: l.view(), history: l.history.Snapshot()}
				continue
			case "history-clear":
				l.history.Clear()
				r.reply <- response{snapshot: l.view()}
				continue
			case "config-export":
				r.reply <- response{snapshot: l.view(), configuration: config.Clone(l.cfg)}
				continue
			}
			err := l.execute(r.cmd, clock.Elapsed())
			l.recordControl(r.cmd, err)
			l.publish(previous)
			r.reply <- response{snapshot: l.view(), err: err}
		case result := <-l.results:
			now = clock.Elapsed()
			l.receive(result, now)
			_ = l.reconcile(now)
			l.publish(previous)
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			now = clock.Elapsed()
			l.event(event, now)
			if event == model.Resume && hasSuspendClock {
				if current, err := suspendClock.SleepTime(); err == nil {
					lastSleep = current
				}
			}
			l.poll(now)
			_ = l.reconcile(now)
			l.publish(previous)
		case <-timer.C():
			now = clock.Elapsed()
			resumed := false
			if hasSuspendClock {
				if current, err := suspendClock.SleepTime(); err == nil {
					resumed = resumed || current-lastSleep > 100*time.Millisecond
					lastSleep = current
				}
			}
			if resumed {
				l.event(model.Resume, now)
			}
			l.readBattery()
			l.poll(now)
			_ = l.reconcile(now)
			l.publish(previous)
			timer = clock.NewTimer(100 * time.Millisecond)
		}
	}
}
