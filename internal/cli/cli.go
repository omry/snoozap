package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

type Factory func(context.Context, string) (model.Service, error)

const usage = "Usage: snoozap run [--data-dir path] | config check <path>\nConsole commands: mode auto|awake|allow-sleep [--for duration], monitor on|off, config check|apply <path>, status [--json], stop\n"

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, start Factory) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "help") {
		fmt.Fprint(out, usage)
		return 0
	}
	if len(args) == 3 && args[0] == "config" && args[1] == "check" {
		f, err := os.Open(args[2])
		if err != nil {
			fmt.Fprintln(stderr, "Cannot read configuration")
			return 2
		}
		defer f.Close()
		if _, err := config.Parse(f); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(out, "Configuration valid")
		return 0
	}
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("data-dir", "", "current-user configuration directory")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return 2
	}
	service, err := start(ctx, *dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	exit := console(ctx, service, in, out, stderr)
	if err := service.Close(context.Background()); err != nil {
		fmt.Fprintln(stderr, "Runtime cleanup failed:", err)
		return 1
	}
	return exit
}

type input struct {
	line string
	err  error
	eof  bool
}

func console(ctx context.Context, service model.Service, in io.Reader, out, stderr io.Writer) int {
	if _, err := fmt.Fprintln(out, "SnooZap manual console. Enter mode awake [--for 15m], mode auto, mode allow-sleep, monitor on|off, config check|apply <path>, status --json, or stop."); err != nil {
		return 1
	}
	initial, err := service.Snapshot(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if initial.ConfigurationError != "" {
		fmt.Fprintln(stderr, "Configuration rejected; automatic work is inactive:", initial.ConfigurationError)
	}
	lines := make(chan input)
	reading, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 1024), 64*1024)
		for scanner.Scan() {
			select {
			case lines <- input{line: scanner.Text()}:
			case <-reading.Done():
				return
			}
		}
		select {
		case lines <- input{eof: true, err: scanner.Err()}:
		case <-reading.Done():
		}
	}()
	exitCode := 0
	for {
		select {
		case <-ctx.Done():
			return exitCode
		case line := <-lines:
			if line.eof {
				if line.err != nil {
					fmt.Fprintln(stderr, "Console input failed")
					return 1
				}
				return exitCode
			}
			args := strings.Fields(line.line)
			if len(args) == 0 {
				continue
			}
			if len(args) == 1 && args[0] == "stop" {
				return exitCode
			}
			cmd := model.Command{Operation: args[0]}
			jsonOutput := false
			switch {
			case args[0] == "mode" && (len(args) == 2 || len(args) == 4) && model.Mode(args[1]).Valid():
				cmd.Mode = model.Mode(args[1])
				if len(args) == 4 {
					duration, err := time.ParseDuration(args[3])
					if args[2] != "--for" || err != nil || duration <= 0 || cmd.Mode == model.Auto {
						fmt.Fprintln(stderr, "Invalid mode duration")
						exitCode = 2
						continue
					}
					cmd.Duration = duration
				}
			case args[0] == "monitor" && len(args) == 2 && (args[1] == "on" || args[1] == "off"):
				cmd.Operation = "monitor"
				cmd.Monitoring = args[1] == "on"
			case args[0] == "config" && len(args) >= 3 && (args[1] == "check" || args[1] == "apply"):
				prefix := "config " + args[1] + " "
				path := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line.line), prefix))
				if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
					path = path[1 : len(path)-1]
				}
				f, err := os.Open(path)
				if err != nil {
					fmt.Fprintln(stderr, "Cannot read configuration")
					exitCode = 2
					continue
				}
				candidate, parseErr := config.Parse(f)
				f.Close()
				if parseErr != nil {
					fmt.Fprintln(stderr, parseErr)
					exitCode = 2
					continue
				}
				if args[1] == "check" {
					fmt.Fprintln(out, "Configuration valid")
					continue
				}
				cmd.Operation = "config"
				cmd.Configuration = &candidate
			case args[0] == "status" && (len(args) == 1 || len(args) == 2 && args[1] == "--json"):
				jsonOutput = len(args) == 2
			default:
				fmt.Fprint(stderr, usage)
				if exitCode != 1 {
					exitCode = 2
				}
				continue
			}
			s, err := service.Execute(ctx, cmd)
			if err != nil {
				if errors.Is(err, model.ErrStopped) {
					fmt.Fprintln(stderr, err)
					return 1
				}
				fmt.Fprintln(stderr, "Command failed:", err)
				exitCode = 1
			}
			if jsonOutput {
				if err := json.NewEncoder(out).Encode(s); err != nil {
					return 1
				}
			} else {
				if _, err := fmt.Fprintf(out, "mode=%s desired=%t native_request_active=%t accepted=%t: %s\n", s.Mode, s.DesiredHold, s.NativeRequestActive, s.NativeRequestAccepted, s.Reason); err != nil {
					return 1
				}
			}
		}
	}
}
