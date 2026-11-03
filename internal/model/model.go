package model

import (
	"context"
	"errors"
	"time"
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

type PowerBackend interface {
	SetRequired(context.Context, bool) error
	Close() error
}

type Command struct {
	Operation string
	Mode      Mode
}

type Snapshot struct {
	Sequence              uint64    `json:"sequence"`
	Mode                  Mode      `json:"mode"`
	Monitoring            bool      `json:"monitoring"`
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
	Close(context.Context) error
}
