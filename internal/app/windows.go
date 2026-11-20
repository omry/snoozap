//go:build windows

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
	"github.com/omry/snoozap/internal/platform/windows"
	"github.com/omry/snoozap/internal/probe"
	t3probe "github.com/omry/snoozap/internal/probe/t3"
)

type runtimeOwner struct {
	model.Service
	owner       *windows.Owner
	lifecycle   *windows.Lifecycle
	listener    io.Closer
	once        sync.Once
	err         error
	setup       sync.Mutex
	setupCtx    context.Context
	setupCancel context.CancelFunc
	processes   *windows.Processes
}

func (r *runtimeOwner) SetupApplication(ctx context.Context, request model.ApplicationRequest) (model.ApplicationResult, error) {
	value := model.ApplicationResult{Items: []string{}}
	if !request.Valid() {
		return value, model.ErrInvalidInput
	}
	if !r.setup.TryLock() {
		return value, model.ErrUnavailable
	}
	defer r.setup.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	stop := context.AfterFunc(r.setupCtx, cancel)
	defer stop()
	if _, err := r.Service.Snapshot(ctx); err != nil {
		return value, model.ErrUnavailable
	}
	wsl := windows.WSL{Distribution: request.Distribution}
	var err error
	switch request.Action {
	case "sources":
		value.Items, err = wsl.ApplicationSources(ctx)
	case "list":
		if request.Distribution == "" {
			value.Items, err = r.processes.Applications(ctx)
		} else {
			value.Items, err = wsl.Applications(ctx)
		}
	case "test":
		var reader model.ProcessReader = r.processes
		if request.Distribution != "" {
			reader = wsl
		}
		observation := reader.Match(ctx, "", request.Executable)
		value.Observation = &observation
	}
	if err != nil || ctx.Err() != nil {
		return model.ApplicationResult{}, model.ErrUnavailable
	}
	return value, nil
}

func (r *runtimeOwner) Close(ctx context.Context) error {
	err := r.Service.Close(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	r.setupCancel()
	r.once.Do(func() { r.err = errors.Join(r.listener.Close(), r.lifecycle.Close(), r.owner.Close()) })
	return errors.Join(err, r.err)
}
func Start(ctx context.Context, dir string) (model.Service, error) {
	owner, err := windows.AcquireOwner()
	if err != nil {
		return nil, err
	}
	power, err := windows.NewPower()
	if err != nil {
		owner.Close()
		return nil, err
	}
	lifecycle, err := windows.NewLifecycle()
	if err != nil {
		power.Close()
		owner.Close()
		return nil, err
	}
	processes, err := windows.NewProcesses()
	if err != nil {
		lifecycle.Close()
		power.Close()
		owner.Close()
		return nil, err
	}
	var configErr error
	if dir == "" {
		dir, configErr = os.UserCacheDir()
		dir = filepath.Join(dir, "SnooZap")
	}
	cfg := config.Empty()
	if configErr == nil {
		cfg, configErr = windows.LoadConfig(dir)
	}
	message := ""
	if configErr != nil {
		cfg = config.Empty()
		message = "Startup configuration rejected; automatic work is inactive"
	}
	reader := windows.SystemPower{}
	registry := probe.Registry{
		Processes:   processes,
		WSL:         func(distribution string) model.ProcessReader { return windows.WSL{Distribution: distribution} },
		Power:       reader,
		T3:          &t3probe.Registry{Read: func() ([]byte, error) { return windows.LoadT3Bindings(dir) }, Secrets: windows.NewCredentials(), Owner: windows.VerifyT3Owner},
		Credentials: windows.NewCredentials(),
	}
	store := config.Store{Replace: func(data []byte) error { return windows.SaveConfig(dir, data) }}
	options := engine.Options{Configuration: cfg, ReadPower: reader.ReadPower, Save: store.Save, Events: lifecycle.Events, Resolve: registry.Resolve}
	service := engine.New(ctx, power, windows.Clock{}, message, options)
	setupCtx, setupCancel := context.WithCancel(ctx)
	runtime := &runtimeOwner{Service: service, owner: owner, lifecycle: lifecycle, setupCtx: setupCtx, setupCancel: setupCancel, processes: processes}
	listener, err := ipc.Listen(ctx, runtime)
	if err != nil {
		setupCancel()
		service.Close(context.Background())
		lifecycle.Close()
		owner.Close()
		return nil, err
	}
	runtime.listener = listener
	return runtime, nil
}
