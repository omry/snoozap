package engine

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/omry/snoozap/internal/model"
)

func expiredReleaseProbes(v truth, probes map[string]*probeState, now time.Duration) []string {
	ids := []string{}
	for id := range v.unknown {
		p := probes[id]
		if p != nil && p.spec.OnError == "release_after" && p.failing && now-p.failureSince >= time.Duration(p.spec.GraceSeconds)*time.Second {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
func (l *loop) readBattery() {
	b := model.Battery{Source: "unknown"}
	var err error
	if l.options.ReadPower == nil {
		err = errors.New("power telemetry unavailable")
	} else {
		ctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
		b, err = l.options.ReadPower(ctx)
		cancel()
	}
	if err != nil || !b.Known {
		b = model.Battery{Source: "unknown", Blocked: true, Reason: "Power telemetry unavailable"}
	} else {
		policy := l.cfg.Settings.Battery
		switch {
		case b.Source == "ac":
			l.batteryLatched = false
		case policy.ACOnly:
			b.Blocked = true
			b.Reason = "AC power required"
		case !b.Present:
			l.batteryLatched = false
		case b.Source != "battery" || b.Percent == nil || *b.Percent < 0 || *b.Percent > 100:
			b.Blocked = true
			b.Reason = "Battery telemetry unavailable"
		default:
			if *b.Percent <= policy.FloorPercent {
				l.batteryLatched = true
			}
			if *b.Percent >= policy.ResumePercent {
				l.batteryLatched = false
			}
			if l.batteryLatched {
				b.Blocked = true
				b.Reason = "Battery floor protection"
			}
		}
	}
	l.snapshot.Battery = b
}
func (l *loop) reconcile(now time.Duration) error {
	s := &l.snapshot
	if l.deadlineSet && now >= l.deadline {
		s.Mode = model.Auto
		l.deadlineSet = false
		s.Deadline = nil
	}
	s.RemainingMS = 0
	if l.deadlineSet {
		s.RemainingMS = (l.deadline - now).Milliseconds()
	}
	s.Activity = model.Unknown
	s.Rules = []model.RuleStatus{}
	reasons := []string{}
	expiredReasons := []string{}
	if s.Monitoring {
		s.Activity = model.Inactive
		for _, r := range l.cfg.Rules {
			if !r.Enabled {
				continue
			}
			v := evaluate(r.When, l.probes)
			previous := l.holds[r.ID]
			hold, startup := false, false
			switch v.state {
			case model.Active:
				hold = true
				s.Activity = model.Active
			case model.Unknown:
				hold, startup = retainUnknown(v, previous, l.probes, now)
				if s.Activity != model.Active {
					s.Activity = model.Unknown
				}
			}
			startup = startup || hold && v.state == model.Unknown && l.startup[r.ID]
			l.startup[r.ID] = startup
			l.holds[r.ID] = hold
			s.Rules = append(s.Rules, model.RuleStatus{ID: r.ID, State: v.state, Retained: hold && v.state == model.Unknown && !startup, Startup: startup})
			if !hold && v.state == model.Unknown {
				for _, id := range expiredReleaseProbes(v, l.probes, now) {
					expiredReasons = append(expiredReasons, "release_after expired: rule:"+r.ID+" probe:"+id)
				}
			}
			if hold {
				prefix := "rule:"
				if startup {
					prefix = "startup:"
				} else if v.state == model.Unknown {
					prefix = "retained:"
				}
				reasons = append(reasons, prefix+r.ID)
			}
		}
	}
	if s.Mode == model.Awake {
		reasons = []string{"manual"}
	} else if s.Mode == model.AllowSleep {
		reasons = []string{}
	}
	if s.Battery.Blocked || l.suspended {
		reasons = []string{}
	}
	s.Hold = model.Hold{Requested: len(reasons) > 0, Reasons: reasons}
	s.DesiredHold = s.Hold.Requested
	s.Reason = "No automatic rules are configured"
	switch {
	case l.suspended:
		s.Reason = "System suspended; protection released"
	case s.Battery.Blocked:
		s.Reason = s.Battery.Reason
	case s.Mode == model.Awake:
		s.Reason = "Manual Stay awake"
	case s.Mode == model.AllowSleep:
		s.Reason = "Allow sleep releases SnooZap requests; it does not issue Sleep"
	case len(reasons) > 0:
		s.Reason = strings.Join(reasons, ", ")
	case len(expiredReasons) > 0:
		s.Reason = strings.Join(expiredReasons, ", ")
	case !s.Monitoring:
		s.Reason = "Monitoring off"
	case len(s.Rules) > 0 && s.Activity == model.Inactive:
		s.Reason = "Automatic rules inactive"
	case s.Activity == model.Unknown:
		s.Reason = "Not confirmed — no automatic hold"
	}
	var err error
	// Resume may need one balanced clear followed by one fresh acquisition.
	// A failed call stops the pair and retains its bounded retry deadline.
	for attempt := 0; attempt < 2; attempt++ {
		target := s.DesiredHold
		if l.refreshRequired {
			if s.NativeRequestActive {
				target = false
			} else {
				l.refreshRequired = false
			}
		}
		if l.retryDesired != target {
			l.retryAt = 0
			l.retryStep = 0
			l.retryDesired = target
		}
		if s.NativeRequestActive == target || now < l.retryAt {
			break
		}
		err = l.power.SetRequired(l.ctx, target)
		if err != nil {
			s.NativeError = "Windows power request failed"
			delays := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 30 * time.Second}
			l.retryAt = dueAfter(now, delays[l.retryStep])
			if l.retryStep < len(delays)-1 {
				l.retryStep++
			}
			break
		}
		s.NativeRequestActive = target
		if !target {
			l.refreshRequired = false
		}
		s.NativeError = ""
		l.retryAt = 0
		l.retryStep = 0
	}
	if s.NativeRequestActive == s.DesiredHold && !l.refreshRequired {
		s.NativeError = ""
	}
	s.NativeRequestAccepted = s.DesiredHold && s.NativeRequestActive && !l.refreshRequired && s.NativeError == ""
	s.Hold.OSAccepted = s.NativeRequestAccepted
	return err
}
