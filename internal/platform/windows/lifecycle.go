//go:build windows

package windows

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

type nativeTimer struct{ *time.Timer }

func (t nativeTimer) C() <-chan time.Time { return t.Timer.C }

type Clock struct{}

func (Clock) Now() time.Time { return time.Now() }
func (Clock) Elapsed() time.Duration {
	ticks, _, _ := kernel.NewProc("GetTickCount64").Call()
	return time.Duration(ticks) * time.Millisecond
}
func (Clock) NewTimer(d time.Duration) model.Timer { return nativeTimer{time.NewTimer(d)} }
func (Clock) SleepTime() (time.Duration, error) {
	before := (Clock{}).Elapsed()
	var awake uint64
	ok, _, _ := kernel.NewProc("QueryUnbiasedInterruptTime").Call(uintptr(unsafe.Pointer(&awake)))
	after := (Clock{}).Elapsed()
	if ok == 0 || after-before > 50*time.Millisecond || awake > uint64(time.Duration(1<<63-1)/100) {
		return 0, errors.New("periodic suspend detection unavailable")
	}
	return after - time.Duration(awake)*100, nil
}

type Lifecycle struct {
	Events    chan model.LifecycleEvent
	handles   []uintptr
	recipient *subscription
	stopped   atomic.Bool
	once      sync.Once
	err       error
}
type subscription struct{ Callback, Context uintptr }

var powerLibrary = syscall.NewLazyDLL("powrprof.dll")

func NewLifecycle() (*Lifecycle, error) {
	l := &Lifecycle{Events: make(chan model.LifecycleEvent, 16)}
	callback := syscall.NewCallback(func(_ uintptr, event uintptr, _ uintptr) uintptr {
		if l.stopped.Load() {
			return 0
		}
		value := model.PowerChanged
		switch event {
		case 4:
			value = model.Suspend
		case 7, 18:
			value = model.Resume
		}
		select {
		case l.Events <- value:
		default:
		}
		return 0
	})
	l.recipient = &subscription{Callback: callback}
	var handle uintptr
	code, _, _ := powerLibrary.NewProc("PowerRegisterSuspendResumeNotification").Call(2, uintptr(unsafe.Pointer(l.recipient)), uintptr(unsafe.Pointer(&handle)))
	runtime.KeepAlive(l.recipient)
	if code != 0 {
		return nil, errors.New("suspend/resume notifications unavailable")
	}
	l.handles = append(l.handles, handle)
	// PowerSettingRegisterNotification invokes the same callback on source/charge changes.
	guids := []syscall.GUID{{Data1: 0x5d3e9a59, Data2: 0xe9d5, Data3: 0x4b00, Data4: [8]byte{0xa6, 0xbd, 0xff, 0x34, 0xff, 0x51, 0x65, 0x48}}, {Data1: 0xa7ad8041, Data2: 0xb45a, Data3: 0x4cae, Data4: [8]byte{0x87, 0xa3, 0xee, 0xcb, 0xb4, 0x68, 0xa9, 0xe1}}}
	for index := range guids {
		handle = 0
		code, _, _ = powerLibrary.NewProc("PowerSettingRegisterNotification").Call(uintptr(unsafe.Pointer(&guids[index])), 2, uintptr(unsafe.Pointer(l.recipient)), uintptr(unsafe.Pointer(&handle)))
		runtime.KeepAlive(guids)
		runtime.KeepAlive(l.recipient)
		if code != 0 {
			_ = l.Close()
			return nil, errors.New("power-change notifications unavailable")
		}
		l.handles = append(l.handles, handle)
	}
	return l, nil
}
func (l *Lifecycle) Close() error {
	l.once.Do(func() {
		l.stopped.Store(true)
		for index, h := range l.handles {
			name := "PowerSettingUnregisterNotification"
			if index == 0 {
				name = "PowerUnregisterSuspendResumeNotification"
			}
			code, _, _ := powerLibrary.NewProc(name).Call(h)
			if code != 0 {
				l.err = errors.New("power notification cleanup failed")
			}
		}
		runtime.KeepAlive(l.recipient)
	})
	return l.err
}

var security = syscall.NewLazyDLL("advapi32.dll")

func noRedirect(path string) error {
	full, err := filepath.Abs(path)
	if err != nil || strings.HasPrefix(full, `\\`) {
		return errors.New("configuration requires a local absolute path")
	}
	for current := full; ; current = filepath.Dir(current) {
		p, err := syscall.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attrs, _, callErr := kernel.NewProc("GetFileAttributesW").Call(uintptr(unsafe.Pointer(p)))
		runtime.KeepAlive(p)
		if attrs == 0xffffffff {
			if callErr != syscall.ERROR_FILE_NOT_FOUND && callErr != syscall.ERROR_PATH_NOT_FOUND {
				return errors.New("cannot inspect protected configuration path")
			}
		} else if attrs&0x400 != 0 {
			return errors.New("configuration reparse redirection is not permitted")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func privateACL(path string) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var descriptor unsafe.Pointer
	var owner *syscall.SID
	var acl unsafe.Pointer
	code, _, _ := security.NewProc("GetNamedSecurityInfoW").Call(uintptr(unsafe.Pointer(p)), 1, 5, uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&acl)), 0, uintptr(unsafe.Pointer(&descriptor)))
	runtime.KeepAlive(p)
	if code != 0 {
		return errors.New("configuration access policy unavailable")
	}
	defer kernel.NewProc("LocalFree").Call(uintptr(descriptor))
	if owner == nil || acl == nil {
		return errors.New("configuration access policy is not private")
	}
	ownerText, err := owner.String()
	if err != nil || ownerText != sid {
		return errors.New("configuration must belong to the current user")
	}
	count := *(*uint16)(unsafe.Add(acl, 4))
	for i := uint16(0); i < count; i++ {
		var ace unsafe.Pointer
		ok, _, _ := security.NewProc("GetAce").Call(uintptr(acl), uintptr(i), uintptr(unsafe.Pointer(&ace)))
		if ok == 0 || ace == nil {
			return errors.New("cannot inspect configuration permissions")
		}
		header := (*[4]byte)(ace)
		if header[1]&8 != 0 || header[0] == 1 {
			continue
		} // Inherit-only and deny ACEs grant no access here.
		if header[0] != 0 {
			return errors.New("unsupported configuration access policy")
		}
		principal, err := (*syscall.SID)(unsafe.Add(ace, 8)).String()
		if err != nil || principal != sid && principal != "S-1-5-18" && principal != "S-1-5-32-544" {
			return errors.New("configuration grants access to another principal")
		}
	}
	return nil
}
func protectedDirectory(dir string, create bool) error {
	if err := noRedirect(dir); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		sid, sidErr := currentSID()
		if sidErr != nil {
			return sidErr
		}
		text, _ := syscall.UTF16PtrFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
		var descriptor uintptr
		ok, _, _ := security.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
		runtime.KeepAlive(text)
		if ok == 0 {
			return errors.New("cannot create configuration access policy")
		}
		defer kernel.NewProc("LocalFree").Call(descriptor)
		attrs := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: descriptor}
		path, _ := syscall.UTF16PtrFromString(dir)
		ok, _, callErr := kernel.NewProc("CreateDirectoryW").Call(uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&attrs)))
		runtime.KeepAlive(path)
		runtime.KeepAlive(attrs)
		if ok == 0 && callErr != syscall.ERROR_ALREADY_EXISTS {
			return errors.New("cannot create protected configuration directory")
		}
		info, err = os.Stat(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("configuration directory is not a directory")
	}
	return privateACL(dir)
}
func LoadConfig(dir string) (config.Config, error) {
	path := filepath.Join(dir, "config.json")
	if err := noRedirect(path); err != nil {
		return config.Config{}, err
	}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return config.Empty(), nil
	} else if err != nil {
		return config.Config{}, err
	}
	if !info.IsDir() {
		return config.Config{}, errors.New("configuration directory is not a directory")
	}
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		// No protected state is read or created for an empty manual session.
		// Saving settings still requires a private directory and destination.
		return config.Empty(), nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return config.Config{}, errors.New("startup configuration must be a readable regular file")
	}
	if err := protectedDirectory(dir, false); err != nil {
		return config.Config{}, err
	}
	if err := privateACL(path); err != nil {
		return config.Config{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return config.Config{}, errors.New("cannot read startup configuration")
	}
	defer f.Close()
	return config.Parse(f)
}
func SaveConfig(dir string, data []byte) error {
	if err := protectedDirectory(dir, true); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	if err := noRedirect(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("configuration destination is not a regular file")
		}
		if err := privateACL(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".snoozap-config-*")
	if err != nil {
		return errors.New("cannot create configuration candidate")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot flush configuration candidate")
	}
	if err := privateACL(name); err != nil {
		return err
	}
	from, _ := syscall.UTF16PtrFromString(name)
	to, _ := syscall.UTF16PtrFromString(path)
	ok, _, _ := kernel.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 9)
	runtime.KeepAlive(from)
	runtime.KeepAlive(to)
	if ok == 0 {
		return errors.New("cannot replace configuration atomically")
	}
	return nil
}
