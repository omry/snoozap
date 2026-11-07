package probe_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/model"
	"github.com/omry/snoozap/internal/probe"
)

type processReader func(context.Context, string, string) model.Observation

func (f processReader) Match(ctx context.Context, name, path string) model.Observation {
	return f(ctx, name, path)
}

type powerReader func(context.Context) (model.Battery, error)

func (f powerReader) ReadPower(ctx context.Context) (model.Battery, error) { return f(ctx) }

type powerBackend struct{}

func (powerBackend) SetRequired(context.Context, bool) error { return nil }
func (powerBackend) Close() error                            { return nil }

func TestRegistryPreservesAdaptersAndSelectedWSL(t *testing.T) {
	local, selected := 0, 0
	reader := func(calls *int) processReader {
		return func(_ context.Context, name, path string) model.Observation {
			(*calls)++
			if name != "renderer" || path != "" {
				t.Fatal("lost process selector")
			}
			return model.Observation{State: model.Active}
		}
	}
	registry := probe.Registry{Processes: reader(&local), WSL: func(distribution string) model.ProcessReader {
		if distribution != "Ubuntu-24.04" {
			t.Fatal("lost selected distribution")
		}
		return reader(&selected)
	}}
	for _, distribution := range []string{"", "Ubuntu-24.04"} {
		p := registry.Resolve(config.Probe{Type: "process", Name: "renderer", WSLDistribution: distribution})
		if p.Check(context.Background()).State != model.Active {
			t.Fatal("process adapter missing")
		}
	}
	if local != 1 || selected != 1 {
		t.Fatal(local, selected)
	}
	registry.WSL = nil
	if registry.Resolve(config.Probe{Type: "process", WSLDistribution: "missing"}).Check(context.Background()).State != model.Unknown || local != 1 {
		t.Fatal("WSL fell back to native processes")
	}
	if registry.Resolve(config.Probe{Type: "t3", LocalEnvironment: "missing"}).Check(context.Background()).State != model.Unknown {
		t.Fatal("unbound T3 accepted")
	}
	if registry.Resolve(config.Probe{Type: "unknown"}) != nil {
		t.Fatal("unknown adapter accepted")
	}
	for _, test := range []struct {
		source string
		known  bool
		err    error
		state  model.State
	}{
		{"ac", true, nil, model.Active}, {"battery", true, nil, model.Inactive}, {"unknown", false, nil, model.Unknown}, {"ac", true, errors.New("unreadable"), model.Unknown},
	} {
		registry.Power = powerReader(func(context.Context) (model.Battery, error) {
			return model.Battery{Source: test.source, Known: test.known}, test.err
		})
		if got := registry.Resolve(config.Probe{Type: "power", Source: "ac"}).Check(context.Background()); got.State != test.state {
			t.Fatal(got)
		}
	}
}

func TestRegistryFreezesHTTPCondition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "true") }))
	defer server.Close()
	p := config.Probe{Type: "http", URL: server.URL, Value: []byte("true"), CredentialHeaders: map[string]string{}}
	resolved := (probe.Registry{}).Resolve(p)
	copy(p.Value, []byte("null"))
	p.CredentialHeaders["Authorization"] = "missing"
	if resolved.Check(context.Background()).State != model.Active {
		t.Fatal("resolved HTTP condition changed with caller memory")
	}
}

func waitState(t *testing.T, e *engine.Engine, state model.State) model.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := e.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.Activity == state && len(s.Probes) == 1 && s.Probes[0].State == state {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("HTTP observation did not converge", state)
	return model.Snapshot{}
}

func TestHTTPRegistryConfigChangesUseExistingEngineAndHistory(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if active.Load() {
			io.WriteString(w, "true")
		} else {
			io.WriteString(w, "false")
		}
	}))
	defer server.Close()
	parse := func(url string) config.Config {
		c, err := config.Parse(strings.NewReader(`{"schema_version":1,"probes":{"web":{"type":"http","url":"` + url + `","interval_ms":50,"timeout_ms":40}},"rules":[{"id":"work","when":{"probe":"web"}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cfg := parse(server.URL)
	registry := probe.Registry{}
	writes := 0
	e := engine.New(context.Background(), powerBackend{}, nil, "", engine.Options{Configuration: cfg, Resolve: registry.Resolve, ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Known: true, Source: "ac"}, nil }, Save: func(config.Config) error { writes++; return nil }})
	defer e.Close(context.Background())
	if s := waitState(t, e, model.Active); !s.Hold.Requested || !s.Hold.OSAccepted {
		t.Fatal("HTTP work did not request hold", s)
	}
	active.Store(false)
	if s := waitState(t, e, model.Inactive); s.Hold.Requested {
		t.Fatal("HTTP inactive did not release hold", s)
	}
	invalid := config.Clone(cfg)
	p := invalid.Probes["web"]
	p.URL = "http://192.168.56.1/"
	invalid.Probes["web"] = p
	if _, err := e.Execute(context.Background(), model.Command{Operation: "config", Configuration: &invalid}); err == nil || writes != 0 {
		t.Fatal("invalid candidate reached storage")
	}
	active.Store(true)
	if _, err := e.Execute(context.Background(), model.Command{Operation: "config", Configuration: &cfg}); err != nil || writes != 1 {
		t.Fatal("valid HTTP config apply", err, writes)
	}
	waitState(t, e, model.Active)
	history, err := e.History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seenActive, seenInactive := false, false
	for _, record := range history.Records {
		if record.Operation == "probe" && record.ProbeID == "web" && record.StatusCode == 200 {
			seenActive = seenActive || record.Result == "active"
			seenInactive = seenInactive || record.Result == "inactive"
		}
	}
	if !seenActive || !seenInactive {
		t.Fatal("HTTP observations missing from bounded history")
	}
}

func TestHTTPPollingRecoversAfterAdversarialJSON(t *testing.T) {
	var calls atomic.Int32
	large := "1e" + strings.Repeat("9", (1<<20)-2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			io.WriteString(w, large)
			return
		}
		io.WriteString(w, "1")
	}))
	defer server.Close()
	c, err := config.Parse(strings.NewReader(`{"schema_version":1,"probes":{"web":{"type":"http","url":"` + server.URL + `","operator":"greater_than","value":0,"interval_ms":100,"timeout_ms":50}},"rules":[{"id":"work","when":{"probe":"web"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(context.Background(), powerBackend{}, nil, "", engine.Options{Configuration: c, Resolve: (probe.Registry{}).Resolve, ReadPower: func(context.Context) (model.Battery, error) {
		return model.Battery{Known: true, Source: "ac"}, nil
	}})
	defer e.Close(context.Background())
	if s := waitState(t, e, model.Active); !s.Hold.Requested || !s.Hold.OSAccepted || calls.Load() < 2 {
		t.Fatal("healthy HTTP polling did not recover after the bounded rejection", s, calls.Load())
	}
}
