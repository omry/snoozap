//go:build windows

package ipc

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/omry/snoozap/internal/model"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")

func init() { listenLocal = listenWindows; dialLocal = dialWindows }

func processSID(process syscall.Handle) (string, error) {
	var token syscall.Token
	if err := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		return "", errors.New("Cannot identify local IPC user")
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", errors.New("Cannot identify local IPC user")
	}
	return user.User.Sid.String()
}
func localIdentity() (string, string, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return "", "", err
	}
	sid, err := processSID(process)
	if err != nil {
		return "", "", err
	}
	return sid, `\\.\pipe\SnooZap.Owner.` + sid, nil
}

func newPipe(name, sid string, first bool) (syscall.Handle, error) {
	path, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	sddl, _ := syscall.UTF16PtrFromString("D:P(A;;GA;;;" + sid + ")")
	var sd uintptr
	ok, _, _ := syscall.NewLazyDLL("advapi32.dll").NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	runtime.KeepAlive(sddl)
	if ok == 0 {
		return 0, errors.New("Cannot restrict local IPC to the current user")
	}
	defer kernel.NewProc("LocalFree").Call(sd)
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}
	flags := uintptr(3 | syscall.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= 0x00080000
	} // FILE_FLAG_FIRST_PIPE_INSTANCE
	h, _, _ := kernel.NewProc("CreateNamedPipeW").Call(uintptr(unsafe.Pointer(path)), flags, 8, 16, 65536, 65536, 5000, uintptr(unsafe.Pointer(&sa))) // PIPE_REJECT_REMOTE_CLIENTS, byte mode
	runtime.KeepAlive(path)
	runtime.KeepAlive(sa)
	if syscall.Handle(h) == syscall.InvalidHandle {
		return 0, errors.New("Cannot create the current-user local IPC endpoint")
	}
	return syscall.Handle(h), nil
}
func acceptPipe(ctx context.Context, h syscall.Handle) error {
	event, _, _ := kernel.NewProc("CreateEventW").Call(0, 1, 0, 0)
	if event == 0 {
		return errors.New("Cannot initialize local IPC acceptance")
	}
	defer syscall.CloseHandle(syscall.Handle(event))
	ov := syscall.Overlapped{HEvent: syscall.Handle(event)}
	ok, _, err := kernel.NewProc("ConnectNamedPipe").Call(uintptr(h), uintptr(unsafe.Pointer(&ov)))
	if ok != 0 || err == syscall.Errno(535) {
		return nil
	} // ERROR_PIPE_CONNECTED
	if err == syscall.Errno(232) || err == syscall.ERROR_BROKEN_PIPE {
		return io.EOF
	} // ERROR_NO_DATA: a queued client already closed its end.
	if err != syscall.ERROR_IO_PENDING {
		return errors.New("Local IPC acceptance failed")
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_, _, _ = kernel.NewProc("CancelIoEx").Call(uintptr(h), uintptr(unsafe.Pointer(&ov)))
		close(cancelled)
	})
	defer func() {
		if !stop() {
			<-cancelled
		}
		runtime.KeepAlive(&ov)
	}()
	var transferred uint32
	ok, _, err = kernel.NewProc("GetOverlappedResult").Call(uintptr(h), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&transferred)), 1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if ok == 0 {
		if err == syscall.Errno(232) || err == syscall.ERROR_BROKEN_PIPE {
			return io.EOF
		}
		return errors.New("Local IPC acceptance failed")
	}
	return nil
}

type pipeServer struct {
	cancel        context.CancelFunc
	cancelWorkers context.CancelFunc
	done          chan struct{}
	workers       sync.WaitGroup
	once          sync.Once
	err           error // Written by the accept loop; read only after done closes.
}

func (s *pipeServer) Close() error {
	s.once.Do(func() {
		s.cancel()
		<-s.done
		// The engine closes first. Give its stop response time to drain; all
		// normal frames already have deadlines. Force cancellation at the bound.
		timer := time.AfterFunc(ioTimeout, s.cancelWorkers)
		s.workers.Wait()
		timer.Stop()
		s.cancelWorkers()
	})
	return s.err
}
func listenWindows(ctx context.Context, service model.Service) (io.Closer, error) {
	sid, name, err := localIdentity()
	if err != nil {
		return nil, err
	}
	first, err := newPipe(name, sid, true)
	if err != nil {
		return nil, err
	}
	acceptCtx, cancel := context.WithCancel(ctx)
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	s := &pipeServer{cancel: cancel, cancelWorkers: cancelWorkers, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		h := first
		defer func() {
			if s.err != nil {
				_, _ = service.Execute(context.Background(), model.Command{Operation: "stop"})
			}
			syscall.CloseHandle(h)
		}()
		slots := make(chan struct{}, 15)
		for {
			select {
			case slots <- struct{}{}:
			case <-acceptCtx.Done():
				return
			}
			acceptErr := acceptPipe(acceptCtx, h)
			if acceptErr != nil && !errors.Is(acceptErr, io.EOF) {
				if acceptCtx.Err() == nil {
					s.err = acceptErr
				}
				return
			}
			if acceptCtx.Err() != nil {
				return
			}
			// The accept loop still owns h. Keep the protected named object alive
			// before either a disconnected client or a worker can close it.
			next, err := newPipe(name, sid, false)
			if err != nil {
				s.err = err
				return
			}
			accepted := h
			h = next
			if errors.Is(acceptErr, io.EOF) {
				syscall.CloseHandle(accepted)
				<-slots
				continue
			}
			file := os.NewFile(uintptr(accepted), name)
			if file == nil || file.SetReadDeadline(time.Now().Add(ioTimeout)) != nil {
				if file != nil {
					file.Close()
				} else {
					syscall.CloseHandle(accepted)
				}
				s.err = errors.New("Local IPC deadlines unavailable")
				return
			}
			s.workers.Add(1)
			go func() { defer s.workers.Done(); defer func() { <-slots }(); ServeConn(workerCtx, file, service) }()
		}
	}()
	return s, nil
}

func dialWindows(ctx context.Context) (Conn, error) {
	sid, name, err := localIdentity()
	if err != nil {
		return nil, err
	}
	path, _ := syscall.UTF16PtrFromString(name)
	deadline := time.Now().Add(ioTimeout)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Identification-only SQOS prevents a server from impersonating this
		// caller with greater authority. Both endpoints use overlapped I/O.
		h, e := syscall.CreateFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED|0x00110000, 0)
		if e == nil {
			var pid uint32
			ok, _, _ := kernel.NewProc("GetNamedPipeServerProcessId").Call(uintptr(h), uintptr(unsafe.Pointer(&pid)))
			if ok == 0 {
				syscall.CloseHandle(h)
				return nil, errors.New("Cannot identify the local SnooZap owner")
			}
			process, e := syscall.OpenProcess(0x1000, false, pid)
			if e != nil {
				syscall.CloseHandle(h)
				return nil, errors.New("Cannot identify the local SnooZap owner")
			}
			peer, e := processSID(process)
			syscall.CloseHandle(process)
			if e != nil || peer != sid {
				syscall.CloseHandle(h)
				return nil, errors.New("Local SnooZap owner identity mismatch")
			}
			f := os.NewFile(uintptr(h), name)
			if f == nil {
				syscall.CloseHandle(h)
				return nil, errors.New("Cannot attach to the local SnooZap owner")
			}
			if f.SetReadDeadline(deadline) != nil || f.SetWriteDeadline(deadline) != nil {
				f.Close()
				return nil, errors.New("Local IPC deadlines unavailable")
			}
			return f, nil
		}
		if e == syscall.ERROR_FILE_NOT_FOUND {
			return nil, model.ErrNoInstance
		}
		if e != syscall.Errno(231) {
			return nil, errors.New("Cannot attach to the local SnooZap owner")
		} // ERROR_PIPE_BUSY
		if !time.Now().Before(deadline) {
			return nil, errors.New("Local SnooZap owner is busy")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
