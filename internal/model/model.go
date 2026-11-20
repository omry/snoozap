package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

var (
	ErrStopped              = errors.New("runtime stopped")
	ErrNoInstance           = errors.New("no running SnooZap instance")
	ErrInvalidInput         = errors.New("invalid command or configuration")
	ErrUnavailable          = errors.New("operation unavailable")
	ErrConfigurationChanged = errors.New("configuration changed while editing")
)

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

// ApplicationSetup is an optional, observational capability of the owner.
// These fixed operations never execute a selected application or change rules.
type ApplicationRequest struct {
	Action       string `json:"action"`
	Distribution string `json:"distribution,omitempty"`
	Executable   string `json:"executable,omitempty"`
}

func (r *ApplicationRequest) UnmarshalJSON(data []byte) error {
	if len(data) > 65536 || config.ValidateJSONEncoding(data) != nil {
		return ErrInvalidInput
	}
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return ErrInvalidInput
	}
	value, seen := ApplicationRequest{}, map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return ErrInvalidInput
		}
		seen[name] = true
		token, err := d.Token()
		text, ok := token.(string)
		if err != nil || !ok {
			return ErrInvalidInput
		}
		switch name {
		case "action":
			value.Action = text
		case "distribution":
			value.Distribution = text
		case "executable":
			value.Executable = text
		default:
			return ErrInvalidInput
		}
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return ErrInvalidInput
	}
	if _, err := d.Token(); err != io.EOF || !value.Valid() {
		return ErrInvalidInput
	}
	*r = value
	return nil
}

func (r ApplicationRequest) Valid() bool {
	if len(r.Distribution) > 255 || !utf8.ValidString(r.Distribution) || strings.HasPrefix(r.Distribution, "-") || strings.ContainsFunc(r.Distribution, unicode.IsControl) {
		return false
	}
	switch r.Action {
	case "sources":
		return r.Distribution == "" && r.Executable == ""
	case "list":
		return r.Executable == ""
	case "test":
		if r.Executable == "" || len(r.Executable) > 32768 || !utf8.ValidString(r.Executable) {
			return false
		}
		cfg := config.Empty()
		cfg.Probes["application"] = config.Probe{Type: "process", Executable: r.Executable, WSLDistribution: r.Distribution,
			IntervalMS: 5000, TimeoutMS: 2000, GraceSeconds: 60, OnStartupUnknown: "wait", OnError: "retain"}
		return config.Validate(cfg) == nil
	}
	return false
}

type ApplicationResult struct {
	Items       []string     `json:"items"`
	Observation *Observation `json:"observation,omitempty"`
}

type ApplicationSetup interface {
	SetupApplication(context.Context, ApplicationRequest) (ApplicationResult, error)
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
	Operation             string
	Mode                  Mode
	Duration              time.Duration
	Monitoring            bool
	Configuration         *config.Config
	ConfigurationRevision *string
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
	SchemaVersion         int           `json:"schema_version"`
	Sequence              uint64        `json:"sequence"`
	ConfigurationRevision string        `json:"configuration_revision"`
	Mode                  Mode          `json:"mode"`
	Monitoring            bool          `json:"monitoring"`
	Activity              State         `json:"activity"`
	Hold                  Hold          `json:"hold"`
	Battery               Battery       `json:"battery"`
	Probes                []ProbeStatus `json:"probes"`
	Rules                 []RuleStatus  `json:"rules"`
	Deadline              *time.Time    `json:"deadline"`
	RemainingMS           int64         `json:"remaining_ms"`
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
	// Subscribe returns an initial snapshot followed by copied latest-only updates.
	// Cancellation detaches the subscriber; owner shutdown closes the channel.
	Subscribe(context.Context) (<-chan Snapshot, error)
	Execute(context.Context, Command) (Snapshot, error)
	History(context.Context) (history.Snapshot, error)
	ClearHistory(context.Context) error
	ExportConfig(context.Context) ([]byte, error)
	Close(context.Context) error
}
