//go:build windows

package windows

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/omry/snoozap/internal/model"
)

type Processes struct{ sid string }

func currentSID() (string, error) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return "", err
	}
	return processSID(h)
}
func processSID(h syscall.Handle) (string, error) {
	var token syscall.Token
	if err := syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String()
}
func NewProcesses() (*Processes, error) {
	sid, err := currentSID()
	if err != nil {
		return nil, errors.New("current-user process identity unavailable")
	}
	return &Processes{sid: sid}, nil
}
func exited(h syscall.Handle) bool {
	var code uint32
	ok, _, _ := kernel.NewProc("GetExitCodeProcess").Call(uintptr(h), uintptr(unsafe.Pointer(&code)))
	return ok != 0 && code != 259
}
func processPath(h syscall.Handle) (string, error) {
	data := make([]uint16, 32768)
	size := uint32(len(data))
	ok, _, _ := kernel.NewProc("QueryFullProcessImageNameW").Call(uintptr(h), 0, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)))
	runtime.KeepAlive(data)
	if ok == 0 || size == 0 || size >= uint32(len(data)) {
		return "", errors.New("process executable unavailable")
	}
	return syscall.UTF16ToString(data[:size]), nil
}

// GetFullPathNameW normalizes Win32 spelling without opening the selected file
// or contacting a UNC server. Compare DOS and extended UNC spellings alike.
func normalizedExecutable(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	data := make([]uint16, 32768)
	size, _, _ := kernel.NewProc("GetFullPathNameW").Call(uintptr(unsafe.Pointer(p)), uintptr(len(data)), uintptr(unsafe.Pointer(&data[0])), 0)
	runtime.KeepAlive(p)
	runtime.KeepAlive(data)
	if size == 0 || size >= uintptr(len(data)) {
		return "", errors.New("process executable spelling unavailable")
	}
	full := syscall.UTF16ToString(data[:size])
	switch {
	case len(full) >= 8 && strings.EqualFold(full[:8], `\\?\UNC\`):
		full = `\\` + full[8:]
	case strings.HasPrefix(full, `\\?\`):
		full = full[4:]
	}
	if strings.HasPrefix(full, `\\.\`) || !(len(full) > 2 && full[1] == ':' && full[2] == '\\' || strings.HasPrefix(full, `\\`)) {
		return "", errors.New("unsupported executable path namespace")
	}
	return full, nil
}
func (p *Processes) Match(ctx context.Context, name, executable string) model.Observation {
	unknown := model.Observation{State: model.Unknown, ErrorCode: "incomplete"}
	if p == nil || p.sid == "" {
		return unknown
	}
	if ctx.Err() != nil {
		return model.Observation{State: model.Unknown, ErrorCode: "cancelled"}
	}
	if executable != "" {
		var err error
		executable, err = normalizedExecutable(executable)
		if err != nil {
			return unknown
		}
	}
	handle, err := syscall.CreateToolhelp32Snapshot(2, 0)
	if err != nil {
		return unknown
	}
	defer syscall.CloseHandle(handle)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = syscall.Process32First(handle, &entry)
	active, incomplete := false, false
	selector := name
	if executable != "" {
		selector = filepath.Base(executable)
	}
	for err == nil {
		if ctx.Err() != nil {
			return model.Observation{State: model.Unknown, ErrorCode: "cancelled"}
		}
		if entry.ProcessID != 0 && strings.EqualFold(syscall.UTF16ToString(entry.ExeFile[:]), selector) {
			process, openErr := syscall.OpenProcess(0x1000, false, entry.ProcessID)
			if openErr != nil {
				if openErr != syscall.Errno(87) {
					incomplete = true
				}
			} else {
				sid, identityErr := processSID(process)
				switch {
				case identityErr != nil:
					if !exited(process) {
						incomplete = true
					}
				case sid != p.sid:
				case executable == "":
					active = true
				default:
					path, pathErr := processPath(process)
					if pathErr == nil {
						path, pathErr = normalizedExecutable(path)
					}
					if pathErr != nil {
						if !exited(process) {
							incomplete = true
						}
					} else if strings.EqualFold(path, executable) {
						active = true
					}
				}
				syscall.CloseHandle(process)
			}
		}
		err = syscall.Process32Next(handle, &entry)
	}
	if err != syscall.ERROR_NO_MORE_FILES || incomplete {
		return unknown
	}
	if active {
		return model.Observation{State: model.Active}
	}
	return model.Observation{State: model.Inactive}
}
