//go:build windows

package windows_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/model"
	native "github.com/omry/snoozap/internal/platform/windows"
)

func requireNative(t *testing.T) {
	t.Helper()
	if os.Getenv("SNOOZAP_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("opt in with SNOOZAP_NATIVE_ACCEPTANCE=1; exercises short-lived native power requests")
	}
}

func TestNativePowerRequestLifetime(t *testing.T) {
	requireNative(t)
	p, err := native.NewPower()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	for _, required := range []bool{false, true, true, false, false, true, false} {
		if err := p.SetRequired(context.Background(), required); err != nil {
			t.Fatalf("SystemRequired=%t: %v", required, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.SetRequired(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled native request accepted")
	}
	if err := p.SetRequired(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal("repeated close failed:", err)
	}
	if err := p.SetRequired(context.Background(), true); err == nil {
		t.Fatal("closed handle accepted a new request")
	}
}

func TestNativeSingletonAndRelease(t *testing.T) {
	requireNative(t)
	o, err := native.AcquireOwner()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	second, err := native.AcquireOwner()
	if second != nil {
		second.Close()
		t.Fatal("two same-user owners admitted")
	}
	if !errors.Is(err, native.ErrAlreadyRunning) {
		t.Fatalf("expected existing owner, got %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	newOwner, err := native.AcquireOwner()
	if err != nil {
		t.Fatal("graceful exit left stale owner:", err)
	}
	if err := newOwner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInvalidHandleFailuresAreReported(t *testing.T) {
	requireNative(t)
	var p native.Power
	if err := p.SetRequired(context.Background(), true); err == nil {
		t.Fatal("invalid native request handle reported acceptance")
	}
	if err := p.Close(); err == nil {
		t.Fatal("invalid native handle close reported success")
	}
	var owner native.Owner
	if err := owner.Close(); err != nil {
		t.Fatal("empty owner cleanup is not idempotent:", err)
	}
}

type consoleProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	output *bufio.Scanner
	cancel context.CancelFunc
}

func executable(t *testing.T) string {
	t.Helper()
	p := os.Getenv("SNOOZAP_CONSOLE_EXE")
	if p == "" {
		t.Skip("SNOOZAP_CONSOLE_EXE must name the built Windows console for process acceptance")
	}
	if !filepath.IsAbs(p) {
		t.Fatal("SNOOZAP_CONSOLE_EXE must be absolute")
	}
	return p
}

func launch(t *testing.T, exe, dir string) *consoleProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	args := []string{"run"}
	if dir != "" {
		args = append(args, "--data-dir", dir)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &consoleProcess{cmd: cmd, stdin: in, output: bufio.NewScanner(out), cancel: cancel}
	t.Cleanup(func() {
		in.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		cancel()
	})
	if !p.output.Scan() || !strings.Contains(p.output.Text(), "SnooZap manual console") {
		t.Fatal("console did not start")
	}
	return p
}

func (p *consoleProcess) status(t *testing.T, command string) model.Snapshot {
	t.Helper()
	if command != "" {
		if _, err := fmt.Fprintln(p.stdin, command); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fmt.Fprintln(p.stdin, "status --json"); err != nil {
		t.Fatal(err)
	}
	for p.output.Scan() {
		line := p.output.Text()
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s model.Snapshot
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Fatal("no status response before process exit")
	return model.Snapshot{}
}

func (p *consoleProcess) stop(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(p.stdin, "stop"); err != nil {
		t.Fatal(err)
	}
	p.stdin.Close()
	if err := p.cmd.Wait(); err != nil {
		t.Fatal("graceful stop failed:", err)
	}
	p.cancel()
}

func TestNativeConsoleModesAndDifferentDataDirectory(t *testing.T) {
	requireNative(t)
	exe := executable(t)
	p := launch(t, exe, t.TempDir())
	s := p.status(t, "")
	if s.Mode != model.Auto || s.NativeRequestActive || s.DesiredHold || s.ConfigurationError != "" {
		t.Fatalf("zero-setup startup: %+v", s)
	}
	s = p.status(t, "mode awake")
	if !s.DesiredHold || !s.NativeRequestActive || !s.NativeRequestAccepted {
		t.Fatalf("native manual request not accepted: %+v", s)
	}
	again := p.status(t, "")
	if again.Sequence != s.Sequence {
		t.Fatal("native status observation mutated request state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second := exec.CommandContext(ctx, exe, "run", "--data-dir", t.TempDir())
	second.Stdin = strings.NewReader("stop\n")
	output, err := second.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "already has an owner") {
		t.Fatalf("second data-dir bypassed ownership: %v %s", err, output)
	}
	s = p.status(t, "mode allow-sleep")
	if s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted {
		t.Fatalf("native Allow sleep did not release: %+v", s)
	}
	s = p.status(t, "mode auto")
	if s.Mode != model.Auto || s.NativeRequestActive {
		t.Fatal("Auto did not return to empty-rule idle")
	}
	p.stop(t)
	next := launch(t, exe, t.TempDir())
	next.stop(t)
}

func TestNativeForcedExitReleasesOwner(t *testing.T) {
	requireNative(t)
	exe := executable(t)
	p := launch(t, exe, t.TempDir())
	if !p.status(t, "mode awake").NativeRequestAccepted {
		t.Fatal("cannot test active-owner crash without an accepted native request")
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err == nil {
		t.Fatal("forced exit unexpectedly succeeded")
	}
	p.cancel()
	next := launch(t, exe, t.TempDir())
	s := next.status(t, "")
	if s.Mode != model.Auto || s.DesiredHold || s.NativeRequestActive {
		t.Fatal("restart restored the crashed manual mode")
	}
	next.stop(t)
}

func TestNativeBadStartupConfigDoesNotOverwriteOrDisableManual(t *testing.T) {
	requireNative(t)
	exe := executable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"schema_version":999,"private":"must-not-be-logged"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	p := launch(t, exe, dir)
	s := p.status(t, "")
	if s.ConfigurationError == "" || strings.Contains(s.ConfigurationError, "must-not-be-logged") || s.NativeRequestActive {
		t.Fatalf("bad startup input handling: %+v", s)
	}
	if !p.status(t, "mode awake").NativeRequestAccepted {
		t.Fatal("invalid startup configuration disabled manual control")
	}
	p.stop(t)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) {
		t.Fatal("runtime changed rejected startup configuration")
	}
}

func TestNativeDefaultDirectoryHasNoImplicitWork(t *testing.T) {
	requireNative(t)
	p := launch(t, executable(t), "")
	s := p.status(t, "")
	if s.Mode != model.Auto || s.DesiredHold || s.NativeRequestActive {
		t.Fatal("default local-app-data startup created an implicit hold")
	}
	p.stop(t)
}

func TestNativeNonregularConfigIsRejected(t *testing.T) {
	requireNative(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	p := launch(t, executable(t), dir)
	s := p.status(t, "")
	if s.ConfigurationError == "" || s.NativeRequestActive {
		t.Fatal("directory-valued config did not fail safely")
	}
	p.stop(t)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatal("rejected config directory was replaced")
	}
}

func TestNativeMissingLocalAppDataLeavesManualControlAvailable(t *testing.T) {
	requireNative(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable(t), "run")
	for _, env := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(env), "LOCALAPPDATA=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "LOCALAPPDATA=")
	cmd.Stdin = strings.NewReader("mode awake\nstatus --json\nstop\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("missing path disabled manual runtime: %v %s", err, output)
	}
	found := false
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s model.Snapshot
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		found = true
		if s.ConfigurationError == "" || s.Mode != model.Awake || !s.NativeRequestAccepted {
			t.Fatalf("missing path error or manual acceptance hidden: %+v", s)
		}
	}
	if !found {
		t.Fatal("no status emitted by manual runtime")
	}
}
