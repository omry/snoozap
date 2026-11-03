//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var powerCreate = kernel.NewProc("PowerCreateRequest")
var powerSet = kernel.NewProc("PowerSetRequest")
var powerClear = kernel.NewProc("PowerClearRequest")

const systemRequired = 1

type reasonContext struct {
	Version             uint32
	Flags               uint32
	SimpleReasonString  *uint16
	UnusedReasonID      uint32
	UnusedStringCount   uint32
	UnusedReasonStrings uintptr
}

type Power struct {
	mu     sync.Mutex
	handle syscall.Handle
	active bool
	closed bool
}

func NewPower() (*Power, error) {
	reason, err := syscall.UTF16PtrFromString("SnooZap: selected work requires the computer")
	if err != nil {
		return nil, err
	}
	c := reasonContext{Flags: 1, SimpleReasonString: reason}
	h, _, callErr := powerCreate.Call(uintptr(unsafe.Pointer(&c)))
	runtime.KeepAlive(reason)
	runtime.KeepAlive(c)
	if h == ^uintptr(0) {
		return nil, fmt.Errorf("PowerCreateRequest: %w", callErr)
	}
	return &Power{handle: syscall.Handle(h)}, nil
}

func (p *Power) SetRequired(ctx context.Context, required bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.closed {
		return errors.New("power handle is closed")
	}
	if p.active == required {
		return nil
	}
	proc := powerClear
	if required {
		proc = powerSet
	}
	ok, _, err := proc.Call(uintptr(p.handle), systemRequired)
	if ok == 0 {
		return fmt.Errorf("SystemRequired transition: %w", err)
	}
	p.active = required
	return nil
}

func (p *Power) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	var clearErr error
	if p.active {
		ok, _, err := powerClear.Call(uintptr(p.handle), systemRequired)
		if ok == 0 {
			clearErr = fmt.Errorf("PowerClearRequest: %w", err)
		}
	}
	closeErr := syscall.CloseHandle(p.handle)
	p.closed = true
	p.active = false
	return errors.Join(clearErr, closeErr)
}
