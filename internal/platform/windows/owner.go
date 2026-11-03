//go:build windows

package windows

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var ErrAlreadyRunning = errors.New("SnooZap already has an owner for this Windows user")

type Owner struct {
	mu     sync.Mutex
	handle syscall.Handle
}

func AcquireOwner() (*Owner, error) {
	var token syscall.Token
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, fmt.Errorf("read current process: %w", err)
	}
	if err := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("read current-user identity: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current-user SID: %w", err)
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		return nil, fmt.Errorf("format current-user SID: %w", err)
	}
	name, err := syscall.UTF16PtrFromString(`Global\SnooZap.Owner.` + sid)
	if err != nil {
		return nil, err
	}
	sddl, err := syscall.UTF16PtrFromString("D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	var descriptor uintptr
	convert := syscall.NewLazyDLL("advapi32.dll").NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	ok, _, err := convert.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	runtime.KeepAlive(sddl)
	if ok == 0 {
		return nil, fmt.Errorf("create current-user owner ACL: %w", err)
	}
	defer kernel.NewProc("LocalFree").Call(descriptor)
	attrs := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
	h, _, callErr := kernel.NewProc("CreateMutexW").Call(uintptr(unsafe.Pointer(&attrs)), 0, uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	runtime.KeepAlive(attrs)
	if h == 0 {
		return nil, fmt.Errorf("create current-user owner: %w", callErr)
	}
	if callErr == syscall.ERROR_ALREADY_EXISTS {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, ErrAlreadyRunning
	}
	return &Owner{handle: syscall.Handle(h)}, nil
}

func (o *Owner) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(o.handle)
	o.handle = 0
	return err
}
