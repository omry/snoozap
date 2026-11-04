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
	"unicode"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

type Factory func(context.Context, string) (model.Service, error)
type Connector func(context.Context) (model.Service, error)

const usage = "Usage: snoozap run [--data-dir path] | status [--json] | mode auto|awake|allow-sleep [--for duration] | monitor on|off | config check|apply|export <path> | history [--json]|clear|export <path> | wake list|send <target-id> | stop\nOnly config check and help work without an owner. run --tui and tui become available with the TUI.\n"

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, start Factory, attach ...Connector) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "help") {
		if _, err := fmt.Fprint(out, usage); err != nil {
			return 1
		}
		return 0
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] != "run" {
		c, err := parseCommand(args)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if c.operation == "config-check" {
			code, _ := c.execute(ctx, nil, out, stderr)
			return code
		}
		if len(attach) != 1 || attach[0] == nil {
			fmt.Fprint(stderr, usage)
			return 2
		}
		service, err := attach[0](ctx)
		if err != nil {
			return failure(stderr, err)
		}
		defer service.Close(context.Background())
		code, _ := c.execute(ctx, service, out, stderr)
		return code
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("data-dir", "", "current-user configuration directory")
	tui := flags.Bool("tui", false, "foreground TUI (available with the TUI increment)")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return 2
	}
	if *tui {
		fmt.Fprintln(stderr, "TUI is unavailable in this increment")
		return 1
	}
	service, err := start(ctx, *dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	code := console(ctx, service, in, out, stderr)
	if err := service.Close(context.Background()); err != nil {
		fmt.Fprintln(stderr, "Runtime cleanup failed:", err)
		return 1
	}
	return code
}

type command struct {
	operation string
	cmd       model.Command
	path      string
	json      bool
}

func parseCommand(args []string) (command, error) {
	c := command{}
	invalid := func() (command, error) { return command{}, model.ErrInvalidInput }
	if len(args) == 0 {
		return invalid()
	}
	c.operation = args[0]
	switch {
	case args[0] == "status" && (len(args) == 1 || len(args) == 2 && args[1] == "--json"):
		c.cmd.Operation = "status"
		c.json = len(args) == 2
	case args[0] == "mode" && (len(args) == 2 || len(args) == 4) && model.Mode(args[1]).Valid():
		c.cmd = model.Command{Operation: "mode", Mode: model.Mode(args[1])}
		if len(args) == 4 {
			duration, err := time.ParseDuration(args[3])
			if args[2] != "--for" || err != nil || duration <= 0 || c.cmd.Mode == model.Auto {
				return invalid()
			}
			c.cmd.Duration = duration
		}
	case args[0] == "monitor" && len(args) == 2 && (args[1] == "on" || args[1] == "off"):
		c.cmd = model.Command{Operation: "monitor", Monitoring: args[1] == "on"}
	case args[0] == "stop" && len(args) == 1:
		c.cmd.Operation = "stop"
	case args[0] == "history":
		switch {
		case len(args) == 1:
		case len(args) == 2 && args[1] == "--json":
			c.json = true
		case len(args) == 2 && args[1] == "clear":
			c.operation = "history-clear"
		case len(args) == 3 && args[1] == "export":
			c.operation = "history-export"
			c.path = args[2]
		default:
			return invalid()
		}
	case args[0] == "config" && len(args) == 3:
		c.path = args[2]
		switch args[1] {
		case "export":
			c.operation = "config-export"
		case "check", "apply":
			f, err := os.Open(c.path)
			if err != nil {
				return command{}, fmt.Errorf("%w: cannot read configuration", model.ErrInvalidInput)
			}
			cfg, err := config.Parse(f)
			f.Close()
			if err != nil {
				return command{}, fmt.Errorf("%w: %s", model.ErrInvalidInput, err)
			}
			if args[1] == "check" {
				c.operation = "config-check"
			} else {
				c.cmd = model.Command{Operation: "config", Configuration: &cfg}
			}
		default:
			return invalid()
		}
	case args[0] == "wake" && (len(args) == 2 && args[1] == "list" || len(args) == 3 && args[1] == "send"):
		c.operation = "wake"
	case args[0] == "tui" && len(args) == 1:
		c.operation = "tui"
	default:
		return invalid()
	}
	return c, nil
}
func failure(out io.Writer, err error) int {
	fmt.Fprintln(out, err)
	if errors.Is(err, model.ErrNoInstance) {
		return 3
	}
	if errors.Is(err, model.ErrInvalidInput) {
		return 2
	}
	return 1
}
func (c command) execute(ctx context.Context, s model.Service, out, stderr io.Writer) (int, bool) {
	switch c.operation {
	case "config-check":
		_, err := fmt.Fprintln(out, "Configuration valid")
		if err != nil {
			return 1, true
		}
		return 0, false
	case "wake", "tui":
		// Require an existing owner, while keeping future wake authority unavailable.
		if _, err := s.Snapshot(ctx); err != nil {
			return failure(stderr, err), false
		}
		fmt.Fprintf(stderr, "%s is unavailable in this increment\n", c.operation)
		return 1, false
	case "history-clear":
		if err := s.ClearHistory(ctx); err != nil {
			fmt.Fprintln(stderr, "Cannot clear history")
			return errorCode(err), false
		}
		_, err := fmt.Fprintln(out, "History cleared")
		return outputResult(err)
	case "history", "history-export":
		h, err := s.History(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "Cannot read history")
			return errorCode(err), false
		}
		if c.operation == "history-export" {
			data, err := json.MarshalIndent(h, "", "  ")
			if err == nil {
				err = writeExport(c.path, append(data, '\n'))
			}
			if err != nil {
				fmt.Fprintln(stderr, "Cannot export history; destination must be a new writable file")
				return 1, false
			}
			_, err = fmt.Fprintln(out, "History exported")
			return outputResult(err)
		}
		if c.json {
			return outputResult(json.NewEncoder(out).Encode(h))
		}
		for _, r := range h.Records {
			if _, err := fmt.Fprintf(out, "%s %s %s probe=%s duration_ms=%d\n", r.Time.Format(time.RFC3339), r.Operation, r.Result, r.ProbeID, r.DurationMS); err != nil {
				return 1, true
			}
		}
		return 0, false
	case "config-export":
		data, err := s.ExportConfig(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "Cannot export configuration")
			return errorCode(err), false
		}
		if err = writeExport(c.path, data); err != nil {
			fmt.Fprintln(stderr, "Cannot export configuration; destination must be a new writable file")
			return 1, false
		}
		_, err = fmt.Fprintln(out, "Configuration exported")
		return outputResult(err)
	}
	var snapshot model.Snapshot
	var err error
	if c.operation == "status" {
		snapshot, err = s.Snapshot(ctx)
	} else {
		snapshot, err = s.Execute(ctx, c.cmd)
	}
	code := 0
	if err != nil {
		code = failure(stderr, err)
		if snapshot.SchemaVersion != 1 {
			return code, false
		}
	}
	if c.operation == "stop" {
		return code, true
	}
	if c.json {
		if err := json.NewEncoder(out).Encode(snapshot); err != nil {
			return 1, true
		}
	} else {
		if _, err := fmt.Fprintf(out, "mode=%s desired=%t native_request_active=%t accepted=%t: %s\n", snapshot.Mode, snapshot.DesiredHold, snapshot.NativeRequestActive, snapshot.NativeRequestAccepted, snapshot.Reason); err != nil {
			return 1, true
		}
	}
	return code, false
}
func errorCode(err error) int {
	if errors.Is(err, model.ErrNoInstance) {
		return 3
	}
	if errors.Is(err, model.ErrInvalidInput) {
		return 2
	}
	return 1
}
func outputResult(err error) (int, bool) {
	if err != nil {
		return 1, true
	}
	return 0, false
}

type input struct {
	line string
	err  error
	eof  bool
}

func console(ctx context.Context, service model.Service, in io.Reader, out, stderr io.Writer) int {
	if _, err := fmt.Fprintln(out, "SnooZap manual console. Enter mode awake [--for 15m], mode auto, mode allow-sleep, monitor on|off, config check|apply|export <path>, history [--json]|clear|export <path>, status --json, or stop."); err != nil {
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
	reading, cancel := context.WithCancel(ctx)
	defer cancel()
	updates, err := service.Subscribe(reading)
	if err != nil {
		fmt.Fprintln(stderr, "Cannot observe owner shutdown")
		return 1
	}
	lines := make(chan input)
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
	exit := 0
	for {
		select {
		case <-ctx.Done():
			return exit
		case _, ok := <-updates:
			if !ok {
				return exit
			}
		case line := <-lines:
			if line.eof {
				if line.err != nil {
					fmt.Fprintln(stderr, "Console input failed")
					return 1
				}
				return exit
			}
			args := strings.Fields(line.line)
			if len(args) == 0 {
				continue
			}
			if len(args) >= 3 && (args[0] == "config" || args[0] == "history" && args[1] == "export") {
				args = append(args[:2], consolePath(line.line, args[0]+" "+args[1]+" "))
			}
			c, err := parseCommand(args)
			if err != nil {
				fmt.Fprintln(stderr, err)
				if exit != 1 {
					exit = 2
				}
				continue
			}
			code, fatal := c.execute(ctx, service, out, stderr)
			if code != 0 && (exit != 1 || code == 1) {
				exit = code
			}
			if fatal {
				return exit
			}
		}
	}
}

func consolePath(line, prefix string) string {
	path := strings.TrimSpace(line)
	for range strings.Fields(prefix) {
		i := strings.IndexFunc(path, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		path = strings.TrimSpace(path[i:])
	}
	if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
		path = path[1 : len(path)-1]
	}
	return path
}

// Exports never replace another file or follow an existing destination symlink.
func writeExport(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("export destination unavailable")
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if n, err := f.Write(data); err != nil || n != len(data) {
		return errors.New("cannot write export")
	}
	if err := f.Sync(); err != nil {
		return errors.New("cannot flush export")
	}
	if err := f.Close(); err != nil {
		return errors.New("cannot close export")
	}
	complete = true
	return nil
}
