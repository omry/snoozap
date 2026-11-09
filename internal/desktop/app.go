//go:build windows

package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os/user"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/omry/snoozap/frontend"
	"github.com/omry/snoozap/internal/app"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
	"github.com/omry/snoozap/internal/platform/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// State keeps the last selected mode separate from current connection evidence.
type State struct {
	Snapshot  model.Snapshot
	Connected bool
	Pending   bool
	Message   string
}

// Controller uses one Service. update must return promptly without calling back
// into the controller; the native view posts updates to the Wails UI thread.
type Controller struct {
	service model.Service
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	update  func(State)
	mu      sync.Mutex
	state   State
	// A command reply cannot restore a subscription that has terminated.
	subscriptionEnded bool
	command           sync.Mutex
	once              sync.Once
	err               error
}

func NewController(ctx context.Context, service model.Service, update func(State)) *Controller {
	ctx, cancel := context.WithCancel(ctx)
	c := &Controller{service: service, ctx: ctx, cancel: cancel, done: make(chan struct{}), update: update}
	go c.watch()
	return c
}

func (c *Controller) watch() {
	defer close(c.done)
	updates, err := c.service.Subscribe(c.ctx)
	if err != nil {
		c.disconnect("Owner unavailable; protection is unconfirmed")
		return
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		case s, ok := <-updates:
			if !ok {
				c.disconnect("Owner disconnected; protection is unconfirmed")
				return
			}
			c.publish(s, "", false)
		}
	}
}

func (c *Controller) disconnect(message string) {
	c.mu.Lock()
	c.subscriptionEnded = true
	c.mu.Unlock()
	c.publish(model.Snapshot{}, message, false)
}

func (c *Controller) publish(s model.Snapshot, message string, finish bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return
	}
	if s.SchemaVersion == 1 && !c.subscriptionEnded {
		if s.Sequence >= c.state.Snapshot.Sequence {
			c.state.Snapshot = s
			c.state.Connected = true
		}
	} else {
		c.state.Connected = false
	}
	c.state.Message = message
	if finish {
		c.state.Pending = false
	}
	c.update(c.state)
}

// Execute never queues or replays a mutation. A later snapshot reconciles an
// uncertain result. The engine remains the authority for validation and policy.
func (c *Controller) Execute(cmd model.Command) error {
	if cmd.Operation != "mode" && cmd.Operation != "stop" {
		return model.ErrInvalidInput
	}
	if !c.command.TryLock() {
		return model.ErrUnavailable
	}
	defer c.command.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.state.Pending = true
	c.update(c.state)
	c.mu.Unlock()
	s, err := c.service.Execute(ctx, cmd)
	message := ""
	if err != nil {
		message = "Command not confirmed; check current status before trying again"
		if s.SchemaVersion == 0 {
			// A failed command does not end the separate status subscription.
			c.mu.Lock()
			s = c.state.Snapshot
			c.mu.Unlock()
		}
	}
	c.publish(s, message, true)
	return err
}

// Close stops a locally owned runtime, or only detaches an IPC client. Explicit
// Quit therefore executes the stop command before closing an attached client.
func (c *Controller) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.command.Lock()
		defer c.command.Unlock()
		c.err = c.service.Close(context.Background())
		<-c.done
	})
	return c.err
}

// StatusBridge is the entire web-facing Service surface. In particular, the
// Controller, runtime shutdown, configuration and general dispatch are unbound.
// Private state travels only in the requesting document's HTTP response.
type StatusBridge struct{ controller *Controller }

type StatusView struct {
	State        State
	Presentation Presentation
}

func NewStatusBridge(controller *Controller) *StatusBridge { return &StatusBridge{controller} }

func (b *StatusBridge) Read() StatusView {
	b.controller.mu.Lock()
	defer b.controller.mu.Unlock()
	s := b.controller.state
	return StatusView{State: s, Presentation: Describe(s)}
}

func (b *StatusBridge) Mode(mode model.Mode, duration string) (StatusView, error) {
	if !mode.Valid() || duration != "" && mode != model.Awake {
		return b.Read(), model.ErrInvalidInput
	}
	var d time.Duration
	if duration != "" {
		var err error
		d, err = time.ParseDuration(duration)
		if err != nil || d <= 0 {
			return b.Read(), model.ErrInvalidInput
		}
	}
	err := b.controller.Execute(model.Command{Operation: "mode", Mode: mode, Duration: d})
	return b.Read(), err
}

const statusOrigin = "http://wails.localhost"
const statusMethodPrefix = "github.com/omry/snoozap/internal/desktop.StatusBridge."

// StatusMiddleware precedes every Wails middleware. CORS and window IDs are
// not authorization. Deny all framework operations except these two bindings;
// canonicalise their small envelopes before the framework parses them again.
func StatusMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; object-src 'none'; frame-src 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Host != "" && r.URL.Host != "wails.localhost" || r.Host != "wails.localhost" || r.URL.Scheme != "" && r.URL.Scheme != "http" {
			http.Error(w, "Untrusted application host", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/wails/runtime":
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if r.Method != http.MethodPost || r.Header.Get("Origin") != statusOrigin || r.Header.Get("Sec-Fetch-Site") == "cross-site" || err != nil || media != "application/json" || r.URL.RawQuery != "" {
				http.Error(w, "Untrusted application request", http.StatusForbidden)
				return
			}
			for name := range r.Header {
				if len(name) >= 14 && http.CanonicalHeaderKey(name)[:14] == "X-Wails-Chunk-" {
					http.Error(w, "Chunked calls are unavailable", http.StatusForbidden)
					return
				}
			}
			var call struct {
				Object *int `json:"object"`
				Method *int `json:"method"`
				Args   struct {
					ID     string            `json:"call-id"`
					Name   string            `json:"methodName"`
					Values []json.RawMessage `json:"args"`
				} `json:"args"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&call)
			var extra any
			if err != nil || decoder.Decode(&extra) != io.EOF || call.Object == nil || *call.Object != 0 || call.Method == nil || *call.Method != 0 || len(call.Args.ID) == 0 || len(call.Args.ID) > 64 ||
				!(call.Args.Name == statusMethodPrefix+"Read" && len(call.Args.Values) == 0 || call.Args.Name == statusMethodPrefix+"Mode" && len(call.Args.Values) == 2) {
				http.Error(w, "Unavailable application operation", http.StatusForbidden)
				return
			}
			body, _ := json.Marshal(call)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		case "/", "/index.html", "/status.js", "/styles.css", "/wails/runtime.js", "/wails/transport.js":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "Read-only asset", http.StatusMethodNotAllowed)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A user-launched second process can pass its foreground permission to this
// user's existing Wails instance. Never grant it to an arbitrary process.
func allowInstanceActivation(id string) {
	class, _ := syscall.UTF16PtrFromString("wails-app-" + id + "-sic")
	title, _ := syscall.UTF16PtrFromString("wails-app-" + id + "-siw")
	user32 := syscall.NewLazyDLL("user32.dll")
	hwnd, _, _ := user32.NewProc("FindWindowExW").Call(^uintptr(2), 0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
	if hwnd != 0 {
		var pid uint32
		user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != 0 {
			user32.NewProc("AllowSetForegroundWindow").Call(uintptr(pid))
		}
	}
}

func Run() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	identity, err := user.Current()
	if err != nil || identity.Uid == "" {
		return errors.New("current Windows user could not be identified")
	}
	instanceID := "com.omry.snoozap.tray." + identity.Uid
	allowInstanceActivation(instanceID)
	activate := make(chan struct{}, 1)
	var controller *Controller
	var view *trayView
	var window *application.WebviewWindow
	var closeErr error
	var shuttingDown atomic.Bool
	var a *application.App
	windowReady, quitRequested := false, false // UI thread only.
	showRequested, customRequested := false, false
	quit := func() {
		quitRequested = true
		// Cancelling Wails during asynchronous WebView2 creation aborts its
		// controller with a fatal startup error. The Service has already stopped;
		// finish initialization before destroying the presentation resources.
		if windowReady {
			a.Quit()
		}
	}
	show := func(custom bool) {
		if window == nil {
			return
		}
		if !windowReady {
			showRequested = true
			customRequested = customRequested || custom
			return
		}
		window.Show()
		window.UnMinimise()
		window.Focus()
		// This is a fixed, data-free focus hint, never a snapshot delivery path.
		if custom {
			window.ExecJS("if(location.origin==='http://wails.localhost'){document.getElementById('duration')?.focus()}")
		}
	}
	bridge := &StatusBridge{}
	a = application.New(application.Options{
		Name:     "SnooZap",
		Services: []application.Service{application.NewService(bridge)},
		Assets:   application.AssetOptions{Handler: frontend.Handler(), Middleware: StatusMiddleware},
		Windows: application.WindowsOptions{
			WndClass: "SnooZapTrayWindow",
			WndProcInterceptor: func(_ uintptr, msg uint32, _, lParam uintptr) (uintptr, bool) {
				const (
					wmUserSystray = 0x0401
					ninSelect     = 0x0400
					ninKeySelect  = 0x0401
				)
				if msg == wmUserSystray {
					switch lParam & 0xffff {
					case ninSelect:
						application.InvokeAsync(func() { show(false) })
					case ninKeySelect:
						if view != nil {
							application.InvokeAsync(view.tray.OpenMenu)
						}
					}
				}
				return 0, false
			},
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: instanceID,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				// Activation is the only accepted meaning of this untrusted payload.
				select {
				case activate <- struct{}{}:
				default:
				}
			},
		},
		OnShutdown: func() {
			shuttingDown.Store(true)
			if controller != nil {
				closeErr = controller.Close()
			}
		},
	})
	// New handles repeated UI launches before any runtime resource is created.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := app.Start(ctx, "")
	owned := true
	if errors.Is(err, windows.ErrAlreadyRunning) {
		owned = false
		service, err = ipc.NewClient(ipc.Dial), nil
	}
	if err != nil {
		return err
	}
	window = a.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "SnooZap status", Title: "SnooZap", URL: "/", Width: 620, Height: 720,
		MinWidth: 340, MinHeight: 400, Hidden: true, DevToolsEnabled: false,
		DefaultContextMenuDisabled: true, EnableFileDrop: false,
	})
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if !shuttingDown.Load() {
			window.Hide()
			e.Cancel()
		}
	})
	window.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) {
		application.InvokeAsync(func() {
			windowReady = true
			if quitRequested {
				a.Quit()
			} else if showRequested {
				show(customRequested)
				showRequested, customRequested = false, false
			}
		})
	})
	view = newTray(a, func(cmd model.Command) {
		if cmd.Operation == "stop" {
			err := controller.Execute(cmd)
			if err != nil && !errors.Is(err, model.ErrStopped) && !errors.Is(err, model.ErrNoInstance) {
				return
			}
			application.InvokeAsync(quit)
			return
		}
		_ = controller.Execute(cmd)
	}, func(custom bool) {
		application.InvokeAsync(func() { show(custom) })
	})
	controller = NewController(ctx, service, func(s State) {
		application.InvokeAsync(func() {
			view.render(s)
			if owned && !s.Connected {
				quit()
			}
		})
	})
	bridge.controller = controller
	defer controller.Close()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-activate:
				application.InvokeAsync(func() { show(false) })
			}
		}
	}()
	err = a.Run()
	return errors.Join(err, closeErr, controller.Close())
}
