package engine

import (
	"testing"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

func leaf(id string) config.Expression { return config.Expression{Probe: id} }
func TestThreeValuedRulesAndContributors(t *testing.T) {
	probes := map[string]*probeState{"yes": {state: model.Active}, "no": {state: model.Inactive}, "u": {state: model.Unknown}, "v": {state: model.Unknown}}
	tests := []struct {
		name         string
		expr         config.Expression
		state        model.State
		contributors int
	}{
		{"not unknown", config.Expression{Not: ptrExpr(leaf("u"))}, model.Unknown, 1},
		{"not true", config.Expression{Not: ptrExpr(leaf("yes"))}, model.Inactive, 0},
		{"not false", config.Expression{Not: ptrExpr(leaf("no"))}, model.Active, 0},
		{"false and unknown", config.Expression{All: []config.Expression{leaf("no"), leaf("u")}}, model.Inactive, 0},
		{"true and unknown", config.Expression{All: []config.Expression{leaf("yes"), leaf("u")}}, model.Unknown, 1},
		{"true or unknown", config.Expression{Any: []config.Expression{leaf("yes"), leaf("u")}}, model.Active, 0},
		{"false or unknown", config.Expression{Any: []config.Expression{leaf("no"), leaf("u")}}, model.Unknown, 1},
		{"all true", config.Expression{All: []config.Expression{leaf("yes"), leaf("yes")}}, model.Active, 0},
		{"any false", config.Expression{Any: []config.Expression{leaf("no"), leaf("no")}}, model.Inactive, 0},
		{"two unknown contributors", config.Expression{Any: []config.Expression{leaf("u"), leaf("v")}}, model.Unknown, 2},
		{"duplicate contributor", config.Expression{All: []config.Expression{leaf("u"), leaf("u")}}, model.Unknown, 1},
		{"nested decisive branch", config.Expression{Any: []config.Expression{{All: []config.Expression{leaf("no"), leaf("u")}}, leaf("yes")}}, model.Active, 0},
		{"nested unknown union", config.Expression{Not: ptrExpr(config.Expression{Any: []config.Expression{{All: []config.Expression{leaf("yes"), leaf("u")}}, leaf("v")}})}, model.Unknown, 2},
	}
	// Enable all leaves; their observed states remain the independently supplied inputs.
	for _, p := range probes {
		p.spec.Enabled = true
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluate(test.expr, probes)
			if got.state != test.state || len(got.unknown) != test.contributors {
				t.Fatalf("got %+v; want %s and %d contributors", got, test.state, test.contributors)
			}
		})
	}
}
func ptrExpr(e config.Expression) *config.Expression { return &e }
func TestRetentionNeedsEveryUnknownContributor(t *testing.T) {
	makeProbe := func(policy, startup string, success bool) *probeState {
		return &probeState{spec: config.Probe{Enabled: true, OnError: policy, OnStartupUnknown: startup, GraceSeconds: 10}, state: model.Unknown, failing: true, failureSince: time.Second, everSucceeded: success}
	}
	probes := map[string]*probeState{"a": makeProbe("retain", "hold", false), "b": makeProbe("release_after", "hold", false)}
	v := truth{state: model.Unknown, unknown: map[string]bool{"a": true, "b": true}}
	if hold, startup := retainUnknown(v, false, probes, 0); !hold || !startup {
		t.Fatal("explicit startup policy did not protect unknown work")
	}
	if hold, _ := retainUnknown(v, true, probes, 10*time.Second); !hold {
		t.Fatal("grace ended early")
	}
	if hold, _ := retainUnknown(v, true, probes, 11*time.Second); hold {
		t.Fatal("one expired contributor did not release uncertain rule")
	}
	if hold, _ := retainUnknown(truth{state: model.Unknown, unknown: map[string]bool{"a": true}}, true, probes, 24*time.Hour); !hold {
		t.Fatal("retain policy acquired an invented deadline")
	}
	probes["a"].everSucceeded = true
	if hold, _ := retainUnknown(v, false, probes, 2*time.Second); hold {
		t.Fatal("a previously successful source regained startup protection")
	}
	probes["a"].everSucceeded = false
	probes["b"].spec.OnStartupUnknown = "wait"
	if hold, _ := retainUnknown(v, false, probes, 0); hold {
		t.Fatal("mixed startup policy ignored wait")
	}
	probes["b"].spec.Enabled = false
	if hold, _ := retainUnknown(v, true, probes, 0); hold {
		t.Fatal("disabled source retained a rule")
	}
}

func TestReleaseAfterStartupEpisodeStaysReleasedUntilRecoveryAndReset(t *testing.T) {
	p := &probeState{spec: config.Probe{Enabled: true, OnError: "release_after", OnStartupUnknown: "hold", GraceSeconds: 1}, state: model.Unknown}
	probes := map[string]*probeState{"p": p}
	v := truth{state: model.Unknown, unknown: map[string]bool{"p": true}}
	if hold, startup := retainUnknown(v, false, probes, 0); !hold || !startup {
		t.Fatal("startup policy did not initially hold unknown work")
	}
	p.failing = true
	p.failureSince = 0
	if hold, _ := retainUnknown(v, true, probes, time.Second); hold {
		t.Fatal("expired release_after policy did not release startup protection")
	}
	for _, now := range []time.Duration{time.Second, 2 * time.Second} {
		if hold, startup := retainUnknown(v, false, probes, now); hold || startup {
			t.Fatalf("released startup episode reacquired at %s: hold=%t startup=%t", now, hold, startup)
		}
	}
	p.state = model.Inactive
	p.failing = false
	p.everSucceeded = true
	if hold, startup := retainUnknown(v, false, probes, 24*time.Hour); hold || startup {
		t.Fatal("confirmed recovery regained startup protection")
	}
	p.state = model.Unknown
	p.failing = true
	p.failureSince = 3 * time.Second
	if hold, startup := retainUnknown(v, false, probes, 4*time.Second); hold || startup {
		t.Fatal("post-recovery failure regained startup protection")
	}
	p.everSucceeded = false
	p.failing = false
	if hold, startup := retainUnknown(v, false, probes, 0); !hold || !startup {
		t.Fatal("monitoring reset did not restore startup protection")
	}
}
