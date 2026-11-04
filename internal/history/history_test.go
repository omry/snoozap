package history

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIndependentBoundsAndOrder(t *testing.T) {
	var s Store
	for i := 0; i < 700; i++ {
		s.Add(Record{Time: time.Unix(int64(1000-i), 0), Operation: "probe", ProbeID: "work", Result: "active", DurationMS: int64(i)})
		s.Add(Record{Time: time.Unix(int64(1000-i), 0), Operation: "mode", Result: "awake"})
	}
	h := s.Snapshot()
	if h.SchemaVersion != 1 || len(h.Records) != 1000 {
		t.Fatalf("wrong bounded history: %d", len(h.Records))
	}
	calls, events := 0, 0
	for i, r := range h.Records {
		if i > 0 && r.Sequence <= h.Records[i-1].Sequence {
			t.Fatal("owner order lost during clock rollback")
		}
		if r.Operation == "probe" {
			calls++
			if r.DurationMS < 200 {
				t.Fatal("old call survived eviction")
			}
		} else {
			events++
		}
	}
	if calls != Limit || events != Limit {
		t.Fatalf("independent limits: %d/%d", calls, events)
	}
	for range 600 {
		s.Add(Record{Operation: "probe", Result: "inactive"})
	}
	if got := s.Snapshot(); len(got.Records) != 1000 {
		t.Fatal("call churn evicted state events")
	}
	h.Records[0].Result = "caller-changed"
	for _, r := range s.Snapshot().Records {
		if r.Result == "caller-changed" {
			t.Fatal("snapshot aliases store")
		}
	}
}

func TestSanitizationAtRecordingBoundary(t *testing.T) {
	var s Store
	secret := "Bearer-secret?code=abc\x1b[2J"
	s.Add(Record{Operation: secret, Result: secret})
	s.Add(Record{Time: time.Now(), Operation: "probe", ProbeID: secret, Result: secret, DurationMS: -5, StatusCode: 1000})
	h := s.Snapshot()
	if len(h.Records) != 1 {
		t.Fatal("unknown operation retained")
	}
	r := h.Records[0]
	if r.ProbeID != "" || r.Result != "unavailable" || r.DurationMS != 0 || r.StatusCode != 0 || r.Time.Location() != time.UTC {
		t.Fatalf("unsafe record: %+v", r)
	}
	data, err := json.Marshal(h)
	if err != nil || strings.Contains(string(data), "secret") || strings.Contains(string(data), "code=") {
		t.Fatal("unsafe export", err)
	}
	for _, id := range []string{"", "work_1-a", strings.Repeat("x", 128)} {
		s.Add(Record{Operation: "probe", ProbeID: id, Result: "inactive", StatusCode: 200})
	}
	for _, id := range []string{strings.Repeat("x", 129), "has space", "unicode-é", "line\nfeed"} {
		s.Add(Record{Operation: "probe", ProbeID: id, Result: "inactive"})
	}
	for _, r := range s.Snapshot().Records {
		if !safeID(r.ProbeID) {
			t.Fatal("invalid ID retained")
		}
	}
}

func TestClearAndRestartEmpty(t *testing.T) {
	var s Store
	for i := range 20 {
		s.Add(Record{Operation: "config", Result: "ok", ProbeID: fmt.Sprintf("p%d", i)})
	}
	before := s.Snapshot().Records[19].Sequence
	s.Clear()
	data, err := json.Marshal(s.Snapshot())
	if err != nil || string(data) != `{"schema_version":1,"records":[]}` {
		t.Fatalf("clear: %s %v", data, err)
	}
	if s.calls != (ring{}) || s.events != (ring{}) {
		t.Fatal("clear retains backing records")
	}
	s.Add(Record{Operation: "state", Result: "unknown"})
	if s.Snapshot().Records[0].Sequence <= before {
		t.Fatal("clear reset event identity")
	}
	var restarted Store
	if len(restarted.Snapshot().Records) != 0 {
		t.Fatal("new runtime restored history")
	}
}

func TestClosedCategories(t *testing.T) {
	var s Store
	results := []string{"active", "inactive", "unknown", "ok", "rejected", "timeout", "unavailable", "incomplete", "cancelled", "adapter_failure", "starved", "auto", "awake", "allow-sleep", "on", "off", "requested", "released", "accepted", "blocked"}
	for _, v := range results {
		s.Add(Record{Operation: "probe", Result: v})
	}
	for i, r := range s.Snapshot().Records {
		if r.Result != results[i] {
			t.Fatal("valid category lost", r.Result)
		}
	}
	for _, op := range []string{"state", "hold", "native", "battery", "mode", "monitor", "config"} {
		s.Add(Record{Operation: op, Result: "ok"})
	}
	for _, code := range []int{99, 100, 200, 599, 600, -1} {
		s.Add(Record{Operation: "probe", Result: "unknown", StatusCode: code})
	}
	h := s.Snapshot()
	tail := h.Records[len(h.Records)-6:]
	for i, want := range []int{0, 100, 200, 599, 0, 0} {
		if tail[i].StatusCode != want {
			t.Fatal("invalid protocol status", tail[i])
		}
	}
}
