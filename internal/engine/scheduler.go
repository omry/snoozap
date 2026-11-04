package engine

import (
	"context"
	"sort"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

func dueAfter(now, duration time.Duration) time.Duration {
	if duration > time.Duration(1<<63-1)-now {
		return time.Duration(1<<63 - 1)
	}
	return now + duration
}

type realTimer struct{ *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.Timer.C }

type schedulerClock struct {
	model.Clock
	origin time.Time
}

func (c schedulerClock) Elapsed() time.Duration               { return time.Since(c.origin) }
func (c schedulerClock) NewTimer(d time.Duration) model.Timer { return realTimer{time.NewTimer(d)} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type probeState struct {
	spec                           config.Probe
	state                          model.State
	code                           string
	observed, success              *time.Time
	everSucceeded, failing, wedged bool
	failureSince, next             time.Duration
}
type poll struct {
	id                string
	generation, epoch uint64
	deadline          time.Duration
	cancel            context.CancelFunc
	expired           bool
	abandonAt         time.Duration
}
type result struct {
	job         *poll
	observation model.Observation
	completed   time.Duration
}

// Give a cancelled check one owner tick to return before treating it as wedged.
const cancellationGrace = 100 * time.Millisecond

func (l *loop) unknown(p *probeState, code string, now time.Duration, completed bool) {
	p.state = model.Unknown
	p.code = code
	if !p.failing {
		p.failing = true
		p.failureSince = now
	}
	if completed {
		at := l.clock.Now()
		p.observed = &at
	}
}
func (l *loop) poll(now time.Duration) {
	if !l.snapshot.Monitoring || l.suspended {
		return
	}
	for _, job := range l.jobs {
		if !job.expired && now >= job.deadline {
			job.expired = true
			job.abandonAt = dueAfter(now, cancellationGrace)
			job.cancel()
			if p := l.probes[job.id]; p != nil && job.generation == l.generation && job.epoch == l.epoch {
				l.unknown(p, "timeout", now, true)
			}
		}
		if job.expired && now >= job.abandonAt {
			if p := l.probes[job.id]; p != nil && job.generation == l.generation && job.epoch == l.epoch {
				p.wedged = true
			}
		}
	}
	ids := make([]string, 0, len(l.probes))
	for id := range l.probes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return
	}
	start := l.cursor % len(ids)
	for offset := range len(ids) {
		id := ids[(start+offset)%len(ids)]
		p := l.probes[id]
		if !p.spec.Enabled || p.wedged {
			continue
		}
		if now-p.next > time.Duration(p.spec.TimeoutMS)*time.Millisecond {
			l.unknown(p, "starved", now, false)
		}
		if l.jobs[id] != nil || now < p.next || len(l.jobs) >= 8 {
			continue
		}
		p.next = dueAfter(now, time.Duration(p.spec.IntervalMS)*time.Millisecond)
		var adapter model.Probe
		if l.options.Resolve != nil {
			adapter = l.options.Resolve(p.spec)
		}
		if adapter == nil {
			l.unknown(p, "unavailable", now, true)
			continue
		}
		ctx, cancel := context.WithTimeout(l.ctx, time.Duration(p.spec.TimeoutMS)*time.Millisecond)
		job := &poll{id: id, generation: l.generation, epoch: l.epoch, deadline: dueAfter(now, time.Duration(p.spec.TimeoutMS)*time.Millisecond), cancel: cancel}
		l.jobs[id] = job
		go func() {
			observation := model.Observation{State: model.Unknown, ErrorCode: "adapter_failure"}
			func() { defer func() { _ = recover() }(); observation = adapter.Check(ctx) }()
			if ctx.Err() != nil {
				observation = model.Observation{State: model.Unknown, ErrorCode: "timeout"}
			}
			completed := l.clock.Elapsed()
			select {
			case l.results <- result{job: job, observation: observation, completed: completed}:
			case <-l.ctx.Done():
			}
		}()
	}
	l.cursor = (start + 1) % len(ids)
}
func (l *loop) receive(r result, now time.Duration) {
	r.job.cancel()
	if l.jobs[r.job.id] == r.job {
		delete(l.jobs, r.job.id)
	}
	p := l.probes[r.job.id]
	if p == nil || r.job.generation != l.generation || r.job.epoch != l.epoch || !l.snapshot.Monitoring {
		return
	}
	if r.job.expired {
		// Completion time avoids mistaking a delayed result delivery for a stuck
		// worker. The expired observation itself can never become fresh again.
		p.wedged = r.completed >= r.job.abandonAt
		return
	}
	if r.completed >= r.job.deadline {
		l.unknown(p, "timeout", now, true)
		p.wedged = r.completed >= dueAfter(r.job.deadline, cancellationGrace)
		return
	}
	at := l.clock.Now()
	p.observed = &at
	if r.observation.State != model.Active && r.observation.State != model.Inactive {
		code := r.observation.ErrorCode
		switch code {
		case "timeout", "unavailable", "incomplete", "cancelled", "adapter_failure":
		default:
			code = "unavailable"
		}
		l.unknown(p, code, now, true)
		return
	}
	p.state = r.observation.State
	p.code = ""
	p.failing = false
	p.everSucceeded = true
	p.success = &at
}
func (l *loop) invalidate(now time.Duration, reset bool) {
	l.epoch++
	for _, job := range l.jobs {
		job.cancel()
	}
	for _, p := range l.probes {
		p.next = now
		p.wedged = false
		p.state = model.Unknown
		p.code = "refreshing"
		if reset {
			p.observed = nil
			p.success = nil
			p.everSucceeded = false
			p.failing = false
		}
	}
	if reset {
		l.holds = map[string]bool{}
		l.startup = map[string]bool{}
	}
}
