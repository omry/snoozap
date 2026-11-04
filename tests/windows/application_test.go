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
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/omry/snoozap/internal/cli"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/model"
	native "github.com/omry/snoozap/internal/platform/windows"
	powerprobe "github.com/omry/snoozap/internal/probe/power"
	processprobe "github.com/omry/snoozap/internal/probe/process"
)

// The copied test executable is a harmless current-user application with an
// independently selected executable path. It owns no SnooZap power request.
func TestApplicationFixtureChild(t *testing.T) {
	if os.Getenv("SNOOZAP_APPLICATION_FIXTURE") != "1" {
		t.Skip("fixture process only")
	}
	fmt.Println("fixture ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
func applicationFixture(t *testing.T) (string, func()) {
	t.Helper()
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snoozap-application-fixture.exe")
	if err := os.WriteFile(path, binary, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-test.run=^TestApplicationFixtureChild$")
	cmd.Env = append(os.Environ(), "SNOOZAP_APPLICATION_FIXTURE=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	cleanup := func() {
		if !done {
			input.Close()
			if err := cmd.Wait(); err != nil {
				t.Error(err)
			}
			done = true
		}
	}
	t.Cleanup(func() {
		if !done {
			input.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			done = true
		}
	})
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "fixture ready" {
		t.Fatal("fixture did not start")
	}
	return path, cleanup
}
func applicationStatus(t *testing.T, p *consoleProcess, predicate func(model.Snapshot) bool) model.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := p.status(t, "")
		if predicate(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("native application state did not converge")
	return model.Snapshot{}
}
func TestNativeApplicationRuleStartsAndReleasesHold(t *testing.T) {
	requireNative(t)
	fixture, stopFixture := applicationFixture(t)
	cfg := map[string]any{"schema_version": 1, "probes": map[string]any{"app": map[string]any{"type": "process", "executable": fixture, "interval_ms": 200, "timeout_ms": 100}}, "rules": []any{map[string]any{"id": "work", "when": map[string]any{"probe": "app"}}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "private-config")
	if err := native.SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	p := launch(t, executable(t), dir)
	applicationStatus(t, p, func(s model.Snapshot) bool { return s.Activity == model.Active && s.NativeRequestAccepted })
	stopFixture()
	applicationStatus(t, p, func(s model.Snapshot) bool { return s.Activity == model.Inactive && !s.NativeRequestActive })
	p.stop(t)
}
func TestNativeTimedConsoleAndPersistedMonitoring(t *testing.T) {
	requireNative(t)
	dir := filepath.Join(t.TempDir(), "private-config")
	exe := executable(t)
	p := launch(t, exe, dir)
	s := p.status(t, "mode awake --for 150ms")
	if !s.NativeRequestAccepted || s.Deadline == nil {
		t.Fatal("timed manual hold not accepted:", s)
	}
	applicationStatus(t, p, func(s model.Snapshot) bool { return s.Mode == model.Auto && !s.NativeRequestActive })
	s = p.status(t, "monitor off")
	if s.Monitoring {
		t.Fatal("monitor off rejected:", s)
	}
	s = p.status(t, "mode awake")
	if !s.NativeRequestAccepted {
		t.Fatal("monitor off blocked direct manual hold:", s)
	}
	p.stop(t)
	next := launch(t, exe, dir)
	s = next.status(t, "")
	if s.Monitoring || s.Mode != model.Auto || s.NativeRequestActive {
		t.Fatal("saved monitoring or runtime-only manual mode incorrect:", s)
	}
	next.stop(t)
}
func TestNativePowerTelemetryAndCurrentUserProcess(t *testing.T) {
	requireNative(t)
	battery, err := (native.SystemPower{}).ReadPower(context.Background())
	if err != nil || !battery.Known {
		t.Fatalf("power telemetry: %+v %v", battery, err)
	}
	if battery.Source != "ac" && battery.Source != "battery" && battery.Source != "unknown" {
		t.Fatal("invalid source")
	}
	if !battery.Present && battery.Percent != nil {
		t.Fatal("absent battery has invented percentage")
	}
	reader, err := native.NewProcesses()
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.Match(context.Background(), "", path); got.State != model.Active {
		t.Fatal("current-user test process not found:", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := reader.Match(ctx, "", path); got.State != model.Unknown {
		t.Fatal("cancelled enumeration reported known inactivity")
	}
}

func TestNativeExecutablePathSpellings(t *testing.T) {
	requireNative(t)
	reader, err := native.NewProcesses()
	if err != nil {
		t.Fatal(err)
	}
	path, stop := applicationFixture(t)
	defer stop()
	extended := `\\?\` + path
	// Include a spelling that needs Win32 normalization before name filtering.
	dotted := filepath.Dir(path) + `\unused\..\` + filepath.Base(path) + "."
	for _, selector := range []string{path, extended, dotted, strings.ToUpper(path)} {
		original, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		alias, err := os.Stat(selector)
		if err != nil || !os.SameFile(original, alias) {
			t.Fatalf("invalid same-file fixture %q: %v", selector, err)
		}
		if got := reader.Match(context.Background(), "", selector); got.State != model.Active {
			t.Fatalf("running executable spelling %q not found: %+v", selector, got)
		}
	}
	other := filepath.Join(filepath.Dir(path), "unrelated-snoozap-review-fixture.exe")
	if got := reader.Match(context.Background(), "", other); got.State != model.Inactive {
		t.Fatal("different executable incorrectly matched:", got)
	}
}
func TestNativeProtectedConfigAndLifecycle(t *testing.T) {
	requireNative(t)
	dir := filepath.Join(t.TempDir(), "private-config")
	data := []byte(`{"schema_version":1,"settings":{"monitoring":false}}`)
	if err := native.SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	cfg, err := native.LoadConfig(dir)
	if err != nil || cfg.Settings.Monitoring {
		t.Fatalf("config round trip: %+v %v", cfg, err)
	}
	if _, err := native.LoadConfig(`\\localhost\C$\untrusted`); err == nil {
		t.Fatal("remote config path accepted")
	}
	lifecycle, err := native.NewLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal("repeated notification cleanup:", err)
	}
	clock := native.Clock{}
	if _, err := clock.SleepTime(); err != nil {
		t.Fatal(err)
	}
	if clock.Elapsed() <= 0 || clock.Now().IsZero() {
		t.Fatal("native clock unavailable")
	}
}

func TestNativeUnsafeConfigPermissionsPreserveFile(t *testing.T) {
	requireNative(t)
	dir := filepath.Join(t.TempDir(), "private-config")
	data := []byte(`{"schema_version":1}`)
	if err := native.SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	var token syscall.Token
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	user, err := token.GetTokenUser()
	if err != nil {
		token.Close()
		t.Fatal(err)
	}
	sid, err := user.User.Sid.String()
	token.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately weaken only this test-owned file, retaining current-user full
	// access so cleanup remains possible. The production store must refuse it.
	text, _ := syscall.UTF16PtrFromString("D:P(A;;FA;;;" + sid + ")(A;;FR;;;WD)")
	security := syscall.NewLazyDLL("advapi32.dll")
	var descriptor uintptr
	ok, _, callErr := security.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW").Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	runtime.KeepAlive(text)
	if ok == 0 {
		t.Fatal(callErr)
	}
	defer syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree").Call(descriptor)
	path := filepath.Join(dir, "config.json")
	nativePath, _ := syscall.UTF16PtrFromString(path)
	ok, _, callErr = security.NewProc("SetFileSecurityW").Call(uintptr(unsafe.Pointer(nativePath)), 4, descriptor)
	runtime.KeepAlive(nativePath)
	if ok == 0 {
		t.Fatal(callErr)
	}
	if _, err := native.LoadConfig(dir); err == nil {
		t.Fatal("world-readable config accepted")
	}
	if err := native.SaveConfig(dir, []byte(`{"schema_version":1,"settings":{"monitoring":false}}`)); err == nil {
		t.Fatal("unsafe existing config overwritten")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatal("refused save changed existing file")
	}
}

// These integration fixtures own only their own native request handles. They
// deliberately do not acquire the product singleton, so development checks can
// coexist with a user's active instance. The executable-startup tests above
// remain separate required checks; this fixture does not attest that boundary.
type nativeFixtureService struct {
	model.Service
	lifecycle *native.Lifecycle
}

func (s *nativeFixtureService) Close(ctx context.Context) error {
	err := s.Service.Close(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(err, s.lifecycle.Close())
}
func nativeFixtureServiceFor(ctx context.Context, dir string) (model.Service, error) {
	cfg, err := native.LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	power, err := native.NewPower()
	if err != nil {
		return nil, err
	}
	lifecycle, err := native.NewLifecycle()
	if err != nil {
		power.Close()
		return nil, err
	}
	processes, err := native.NewProcesses()
	if err != nil {
		lifecycle.Close()
		power.Close()
		return nil, err
	}
	telemetry := native.SystemPower{}
	store := config.Store{Replace: func(data []byte) error { return native.SaveConfig(dir, data) }}
	opts := engine.Options{Configuration: cfg, ReadPower: telemetry.ReadPower, Save: store.Save, Events: lifecycle.Events, Resolve: func(p config.Probe) model.Probe {
		switch p.Type {
		case "process":
			return processprobe.Probe{Reader: processes, Name: p.Name, Executable: p.Executable}
		case "power":
			return powerprobe.Probe{Reader: telemetry, Source: p.Source}
		}
		return nil
	}}
	return &nativeFixtureService{Service: engine.New(ctx, power, native.Clock{}, "", opts), lifecycle: lifecycle}, nil
}
func nativeServiceStatus(t *testing.T, s model.Service, predicate func(model.Snapshot) bool) model.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		snapshot, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(snapshot) {
			return snapshot
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("native engine state did not converge")
	return model.Snapshot{}
}
func TestNativeApplicationEngineWithOccupiedOwnerLock(t *testing.T) {
	requireNative(t)
	occupyOwnerLock(t)
	fixture, stopFixture := applicationFixture(t)
	data, err := json.Marshal(map[string]any{"schema_version": 1, "probes": map[string]any{"app": map[string]any{"type": "process", "executable": fixture, "interval_ms": 300, "timeout_ms": 200}}, "rules": []any{map[string]any{"id": "work", "when": map[string]any{"probe": "app"}}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "config")
	if err := native.SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	service, err := nativeFixtureServiceFor(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	nativeServiceStatus(t, service, func(s model.Snapshot) bool {
		return s.Activity == model.Active && s.Hold.Requested && s.Hold.OSAccepted
	})
	stopFixture()
	nativeServiceStatus(t, service, func(s model.Snapshot) bool {
		return s.Activity == model.Inactive && !s.NativeRequestActive && !s.Hold.Requested
	})
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type fixtureConsole struct {
	input  *io.PipeWriter
	output *bufio.Scanner
	done   chan int
}

func nativeFixtureConsole(t *testing.T, dir string) *fixtureConsole {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	console := &fixtureConsole{input: inputWriter, output: bufio.NewScanner(outputReader), done: make(chan int, 1)}
	go func() {
		code := cli.Run(ctx, []string{"run", "--data-dir", dir}, inputReader, outputWriter, io.Discard, nativeFixtureServiceFor)
		outputWriter.Close()
		inputReader.Close()
		console.done <- code
	}()
	t.Cleanup(func() { cancel(); inputWriter.Close(); outputReader.Close() })
	if !console.output.Scan() {
		t.Fatal("native fixture console failed to start")
	}
	return console
}
func (c *fixtureConsole) status(t *testing.T, command string) model.Snapshot {
	t.Helper()
	if command != "" {
		if _, err := fmt.Fprintln(c.input, command); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fmt.Fprintln(c.input, "status --json"); err != nil {
		t.Fatal(err)
	}
	for c.output.Scan() {
		line := c.output.Bytes()
		if len(line) > 0 && line[0] == '{' {
			var s model.Snapshot
			if err := json.Unmarshal(line, &s); err != nil {
				t.Fatal(err)
			}
			return s
		}
	}
	t.Fatal("native fixture console returned no status")
	return model.Snapshot{}
}
func (c *fixtureConsole) stop(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(c.input, "stop"); err != nil {
		t.Fatal(err)
	}
	c.input.Close()
	select {
	case code := <-c.done:
		if code != 0 {
			t.Fatalf("fixture console exit: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fixture console did not stop")
	}
}
func TestNativeConsoleControlsWithOccupiedOwnerLock(t *testing.T) {
	requireNative(t)
	occupyOwnerLock(t)
	dir := filepath.Join(t.TempDir(), "config")
	console := nativeFixtureConsole(t, dir)
	s := console.status(t, "mode awake --for 200ms")
	if !s.NativeRequestAccepted || s.Deadline == nil {
		t.Fatal("native timed hold not accepted:", s)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s = console.status(t, "")
		if s.Mode == model.Auto && !s.NativeRequestActive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Mode != model.Auto || s.NativeRequestActive {
		t.Fatal("native timed hold did not expire:", s)
	}
	s = console.status(t, "monitor off")
	if s.Monitoring {
		t.Fatal("monitoring remained on")
	}
	s = console.status(t, "mode awake")
	if !s.NativeRequestAccepted {
		t.Fatal("manual hold failed with monitoring off")
	}
	console.stop(t)
	cfg, err := native.LoadConfig(dir)
	if err != nil || cfg.Settings.Monitoring {
		t.Fatal("monitoring setting not persisted:", err)
	}
	next := nativeFixtureConsole(t, dir)
	s = next.status(t, "")
	if s.Mode != model.Auto || s.Monitoring || s.NativeRequestActive {
		t.Fatal("runtime-only mode or persisted monitoring incorrect:", s)
	}
	next.stop(t)
}

func occupyOwnerLock(t *testing.T) {
	t.Helper()
	owner, err := native.AcquireOwner()
	if errors.Is(err, native.ErrAlreadyRunning) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
}
