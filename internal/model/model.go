package model

import (
	"context"
	"errors"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/history"
)

type Mode string

const (
	Auto       Mode = "auto"
	Awake      Mode = "awake"
	AllowSleep Mode = "allow-sleep"
)

func (m Mode) Valid() bool { return m == Auto || m == Awake || m == AllowSleep }

var ErrStopped = errors.New("runtime stopped")

type Clock interface{ Now() time.Time }
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Elapsed includes suspend and is independent of calendar-clock adjustments.
type SchedulerClock interface {
	Clock
	Elapsed() time.Duration
	NewTimer(time.Duration) Timer
}

// SuspendClock distinguishes actual suspend from an ordinary scheduling delay.
type SuspendClock interface {
	SleepTime() (time.Duration, error)
}
type PowerBackend interface {
	SetRequired(context.Context, bool) error
	Close() error
}
type State string

const (
	Active   State = "active"
	Inactive State = "inactive"
	Unknown  State = "unknown"
)

type Observation struct {
	State      State
	ErrorCode  string
	StatusCode int
}
type Probe interface {
	Check(context.Context) Observation
}
type Battery struct {
	Source  string `json:"source"`
	Percent *int   `json:"percent"`
	Present bool   `json:"present"`
	Known   bool   `json:"known"`
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason,omitempty"`
}
type PowerReader interface {
	ReadPower(context.Context) (Battery, error)
}
type ProcessReader interface {
	Match(context.Context, string, string) Observation
}
type LifecycleEvent uint8

const (
	Suspend LifecycleEvent = iota + 1
	Resume
	PowerChanged
)

type Command struct {
	Operation     string
	Mode          Mode
	Duration      time.Duration
	Monitoring    bool
	Configuration *config.Config
}
type Hold struct {
	Requested  bool     `json:"requested"`
	OSAccepted bool     `json:"os_accepted"`
	Reasons    []string `json:"reasons"`
}
type ProbeStatus struct {
	ID            string     `json:"id"`
	State         State      `json:"state"`
	ObservedAt    *time.Time `json:"observed_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	ErrorCode     string     `json:"error_code,omitempty"`
}
type RuleStatus struct {
	ID       string `json:"id"`
	State    State  `json:"state"`
	Retained bool   `json:"retained"`
	Startup  bool   `json:"startup"`
}
type Snapshot struct {
	SchemaVersion int           `json:"schema_version"`
	Sequence      uint64        `json:"sequence"`
	Mode          Mode          `json:"mode"`
	Monitoring    bool          `json:"monitoring"`
	Activity      State         `json:"activity"`
	Hold          Hold          `json:"hold"`
	Battery       Battery       `json:"battery"`
	Probes        []ProbeStatus `json:"probes"`
	Rules         []RuleStatus  `json:"rules"`
	Deadline      *time.Time    `json:"deadline"`
	RemainingMS   int64         `json:"remaining_ms"`
	// Retain the delivered console's indicators; they describe this same hold.
	DesiredHold           bool      `json:"desired_hold"`
	NativeRequestActive   bool      `json:"native_request_active"`
	NativeRequestAccepted bool      `json:"native_request_accepted"`
	NativeError           string    `json:"native_error,omitempty"`
	ConfigurationError    string    `json:"configuration_error,omitempty"`
	Reason                string    `json:"reason"`
	UpdatedAt             time.Time `json:"updated_at"`
	Limitations           []string  `json:"limitations"`
}
type Service interface {
	Snapshot(context.Context) (Snapshot, error)
	Execute(context.Context, Command) (Snapshot, error)
	History(context.Context) (history.Snapshot, error)
	ClearHistory(context.Context) error
	ExportConfig(context.Context) ([]byte, error)
	Close(context.Context) error
}
