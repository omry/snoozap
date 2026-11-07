//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/omry/snoozap/internal/app"
	"github.com/omry/snoozap/internal/cli"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, app.Start, func(context.Context) (model.Service, error) {
		return ipc.NewClient(ipc.Dial), nil
	})
	cancel()
	os.Exit(code)
}
