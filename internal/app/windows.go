//go:build windows

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

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
	owner     *windows.Owner
	lifecycle *windows.Lifecycle
	listener  io.Closer
	once      sync.Once
	err       error
}

func (r *runtimeOwner) Close(ctx context.Context) error {
	err := r.Service.Close(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
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
	listener, err := ipc.Listen(ctx, service)
	if err != nil {
		service.Close(context.Background())
		lifecycle.Close()
		owner.Close()
		return nil, err
	}
	return &runtimeOwner{Service: service, owner: owner, lifecycle: lifecycle, listener: listener}, nil
}
