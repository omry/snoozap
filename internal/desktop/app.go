//go:build windows

package desktop

import (
	"context"
	"errors"
	"os/user"
	"runtime"
	"sync"
	"time"

	"github.com/omry/snoozap/internal/app"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
	"github.com/omry/snoozap/internal/platform/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
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

func Run() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	identity, err := user.Current()
	if err != nil || identity.Uid == "" {
		return errors.New("current Windows user could not be identified")
	}
	activate := make(chan struct{}, 1)
	var controller *Controller
	var view *trayView
	var closeErr error
	a := application.New(application.Options{
		Name: "SnooZap",
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
					case ninSelect, ninKeySelect:
						if view != nil {
							application.InvokeAsync(view.tray.OpenMenu)
						}
					}
				}
				return 0, false
			},
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.omry.snoozap.tray." + identity.Uid,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				// Activation is the only accepted meaning of this untrusted payload.
				select {
				case activate <- struct{}{}:
				default:
				}
			},
		},
		OnShutdown: func() {
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
	view = newTray(a, func(cmd model.Command) {
		if cmd.Operation == "stop" {
			err := controller.Execute(cmd)
			if err != nil && !errors.Is(err, model.ErrStopped) && !errors.Is(err, model.ErrNoInstance) {
				return
			}
			a.Quit()
			return
		}
		_ = controller.Execute(cmd)
	})
	controller = NewController(ctx, service, func(s State) {
		application.InvokeAsync(func() {
			view.render(s)
			if owned && !s.Connected {
				a.Quit()
			}
		})
	})
	defer controller.Close()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-activate:
				application.InvokeAsync(view.tray.OpenMenu)
			}
		}
	}()
	err = a.Run()
	return errors.Join(err, closeErr, controller.Close())
}
