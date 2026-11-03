//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"sync"

	"github.com/omry/snoozap/internal/cli"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/model"
	"github.com/omry/snoozap/internal/platform/windows"
)

type runtimeOwner struct {
	model.Service
	owner *windows.Owner
	once  sync.Once
	err   error
}

func (r *runtimeOwner) Close(ctx context.Context) error {
	err := r.Service.Close(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	r.once.Do(func() { r.err = r.owner.Close() })
	return errors.Join(err, r.err)
}

func start(ctx context.Context, dir string) (model.Service, error) {
	owner, err := windows.AcquireOwner()
	if err != nil {
		return nil, err
	}
	power, err := windows.NewPower()
	if err != nil {
		owner.Close()
		return nil, err
	}
	var configErr error
	if dir == "" {
		dir, configErr = os.UserCacheDir()
		dir = filepath.Join(dir, "SnooZap")
	}
	if configErr == nil {
		path := filepath.Join(dir, "config.json")
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err == nil && info.Mode().IsRegular():
			f, openErr := os.Open(path)
			if openErr != nil {
				configErr = errors.New("cannot read startup configuration")
			} else {
				_, configErr = config.Parse(f)
				f.Close()
			}
		default:
			configErr = errors.New("startup configuration must be a readable regular file")
		}
	}
	message := ""
	if configErr != nil {
		message = configErr.Error()
	}
	return &runtimeOwner{Service: engine.New(ctx, power, nil, message), owner: owner}, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, start)
	cancel()
	os.Exit(code)
}
