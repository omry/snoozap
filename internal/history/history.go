// Package history stores only typed, bounded diagnostics. The runtime owner
// serializes access; records never contain raw errors, URLs, bodies or headers.
package history

import (
	"sort"
	"time"
)

const Limit = 500

type Record struct {
	Sequence   uint64    `json:"sequence"`
	Time       time.Time `json:"time"`
	ProbeID    string    `json:"probe_id,omitempty"`
	Operation  string    `json:"operation"`
	DurationMS int64     `json:"duration_ms"`
	Result     string    `json:"result"`
	StatusCode int       `json:"status_code,omitempty"`
}

type Snapshot struct {
	SchemaVersion int      `json:"schema_version"`
	Records       []Record `json:"records"`
}

type ring struct {
	items [Limit]Record
	next  int
	count int
}

func (r *ring) add(v Record) {
	r.items[r.next] = v
	r.next = (r.next + 1) % Limit
	if r.count < Limit {
		r.count++
	}
}

func (r *ring) appendTo(out []Record) []Record {
	for i := 0; i < r.count; i++ {
		out = append(out, r.items[(r.next-r.count+i+Limit)%Limit])
	}
	return out
}

type Store struct {
	calls, events ring
	sequence      uint64
}

func safeID(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func safeResult(s string) string {
	switch s {
	case "active", "inactive", "unknown", "ok", "rejected", "timeout", "unavailable", "incomplete", "cancelled", "adapter_failure", "starved", "auto", "awake", "allow-sleep", "on", "off", "requested", "released", "accepted", "blocked":
		return s
	}
	return "unavailable"
}

// Add rejects unknown operations rather than retaining caller-supplied text.
func (s *Store) Add(v Record) {
	switch v.Operation {
	case "probe", "state", "hold", "native", "battery", "mode", "monitor", "config":
	default:
		return
	}
	if !safeID(v.ProbeID) {
		v.ProbeID = ""
	}
	v.Result = safeResult(v.Result)
	if v.DurationMS < 0 {
		v.DurationMS = 0
	}
	if v.StatusCode < 100 || v.StatusCode > 599 {
		v.StatusCode = 0
	}
	s.sequence++
	v.Sequence = s.sequence
	// Strip location names and monotonic data from the public timestamp.
	v.Time = v.Time.UTC().Round(0)
	if v.Operation == "probe" {
		s.calls.add(v)
	} else {
		s.events.add(v)
	}
}

func (s *Store) Snapshot() Snapshot {
	out := s.calls.appendTo(make([]Record, 0, s.calls.count+s.events.count))
	out = s.events.appendTo(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return Snapshot{SchemaVersion: 1, Records: out}
}

func (s *Store) Clear() {
	s.calls = ring{}
	s.events = ring{}
}
