package engine

import (
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

type truth struct {
	state   model.State
	unknown map[string]bool
}

func evaluate(e config.Expression, probes map[string]*probeState) truth {
	if e.Probe != "" {
		p := probes[e.Probe]
		if p == nil || !p.spec.Enabled || p.state == model.Unknown {
			return truth{model.Unknown, map[string]bool{e.Probe: true}}
		}
		return truth{state: p.state}
	}
	if e.Not != nil {
		v := evaluate(*e.Not, probes)
		if v.state == model.Active {
			v.state = model.Inactive
		} else if v.state == model.Inactive {
			v.state = model.Active
		}
		return v
	}
	children, decisive, identity := e.All, model.Inactive, model.Active
	if e.Any != nil {
		children, decisive, identity = e.Any, model.Active, model.Inactive
	}
	unknown := map[string]bool{}
	for _, child := range children {
		v := evaluate(child, probes)
		if v.state == decisive {
			return truth{state: decisive}
		}
		for id := range v.unknown {
			unknown[id] = true
		}
	}
	if len(unknown) > 0 {
		return truth{model.Unknown, unknown}
	}
	return truth{state: identity}
}
func retainUnknown(v truth, previous bool, probes map[string]*probeState, now time.Duration) (hold, startup bool) {
	if len(v.unknown) == 0 {
		return false, false
	}
	for id := range v.unknown {
		p := probes[id]
		if p == nil || !p.spec.Enabled {
			return false, false
		}
		if p.spec.OnError == "release_after" && p.failing && now-p.failureSince >= time.Duration(p.spec.GraceSeconds)*time.Second {
			return false, false
		}
		if !previous && (p.everSucceeded || p.spec.OnStartupUnknown != "hold") {
			return false, false
		}
	}
	return true, !previous
}
func references(e config.Expression, id string) bool {
	if e.Probe == id {
		return true
	}
	if e.Not != nil && references(*e.Not, id) {
		return true
	}
	for _, group := range [][]config.Expression{e.All, e.Any} {
		for _, child := range group {
			if references(child, id) {
				return true
			}
		}
	}
	return false
}
