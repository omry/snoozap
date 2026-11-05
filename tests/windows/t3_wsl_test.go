//go:build windows

package windows_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omry/snoozap/internal/model"
	native "github.com/omry/snoozap/internal/platform/windows"
	"github.com/omry/snoozap/internal/probe/t3"
)

func wslList(text string) []byte {
	words := utf16.Encode([]rune(text))
	out := make([]byte, len(words)*2)
	for i, v := range words {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

func TestWSLFixedCurrentUserProcessContract(t *testing.T) {
	tests := []struct {
		name, selector, executable, uid, rows string
		state                                 model.State
	}{
		{"active name", "renderer", "", "1000\n", "ps\nrenderer\n", model.Active},
		{"inactive", "renderer", "", "1000\n", "ps\nother\n", model.Inactive},
		{"case sensitive", "renderer", "", "1000\n", "ps\nRenderer\n", model.Inactive},
		{"inspector excluded", "ps", "", "1000\n", "ps\n", model.Inactive},
		{"real ps counted", "ps", "", "1000\n", "ps\nps\n", model.Active},
		{"path spaces", "", "/opt/my app/renderer", "1000\n", "/usr/bin/ps\n/opt/my app/renderer\n", model.Active},
		{"clean path", "", "/opt/../opt/renderer", "1000\n", "/usr/bin/ps\n/opt/renderer\n", model.Active},
		{"bad uid", "renderer", "", "1000\nsecret\n", "ps\n", model.Unknown},
		{"uid prefix", "renderer", "", "01000\n", "ps\n", model.Unknown},
		{"uid missing newline", "renderer", "", "1000", "ps\n", model.Unknown},
		{"truncated", "renderer", "", "1000\n", "ps\nrenderer", model.Unknown},
		{"inaccessible path", "", "/opt/renderer", "1000\n", "/usr/bin/ps\n-\n", model.Unknown},
		{"deleted executable", "", "/opt/renderer", "1000\n", "/usr/bin/ps\n/opt/renderer (deleted)\n", model.Unknown},
		{"known active with inaccessible peer", "", "/opt/renderer", "1000\n", "/usr/bin/ps\n-\n/opt/renderer\n", model.Active},
		{"unrepresentable", "renderer", "", "1000\n", "ps\nbad?name\n", model.Unknown},
		{"invalid encoding", "renderer", "", "1000\n", "ps\n\xff\n", model.Unknown},
		{"inspector missing", "renderer", "", "1000\n", "other\n", model.Unknown},
		{"oversized", "renderer", "", "1000\n", strings.Repeat("x", t3.MaxBytes) + "\n", model.Unknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			w := native.WSL{Distribution: "Ubuntu-24.04", Run: func(ctx context.Context, args []string, input []byte) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if len(input) != 0 {
					t.Fatal("process command got input")
				}
				if reflect.DeepEqual(args, []string{"--list", "--running", "--quiet"}) {
					return wslList("Ubuntu-24.04\r\n"), nil
				}
				if reflect.DeepEqual(args, []string{"--distribution", "Ubuntu-24.04", "--exec", "/usr/bin/id", "-u"}) {
					return []byte(test.uid), nil
				}
				field := "comm="
				if test.executable != "" {
					field = "exe="
				}
				want := []string{"--distribution", "Ubuntu-24.04", "--exec", "/usr/bin/ps", "-ww", "-u", "1000", "-o", field}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("unexpected command%q", args)
				}
				return []byte(test.rows), nil
			}}
			got := w.Match(context.Background(), test.selector, test.executable)
			if got.State != test.state {
				t.Fatalf("%+v", got)
			}
			for i, args := range calls {
				if args[0] == "--distribution" && (i == 0 || calls[i-1][0] != "--list") {
					t.Fatal("inspection without immediate running preflight")
				}
			}
		})
	}
}

func TestWSLStoppedMissingUnreadableAndStopRace(t *testing.T) {
	for _, failure := range []string{"stopped", "missing", "unreadable", "malformed UTF16", "unpaired surrogate", "stop before ps", "id unavailable", "ps unavailable", "cancelled", "invalid distribution"} {
		t.Run(failure, func(t *testing.T) {
			calls, executions := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			w := native.WSL{Distribution: "Ubuntu-24.04", Run: func(ctx context.Context, args []string, _ []byte) ([]byte, error) {
				calls++
				for _, arg := range args {
					if arg == "--terminate" || arg == "--shutdown" {
						t.Fatal("terminated distribution")
					}
				}
				if args[0] == "--list" {
					switch failure {
					case "stopped":
						return nil, nil
					case "missing":
						return wslList("Other\r\n"), nil
					case "unreadable":
						return nil, errors.New("unreadable")
					case "malformed UTF16":
						return []byte{0, 255, 0}, nil
					case "unpaired surrogate":
						return []byte{0, 0xd8}, nil
					case "stop before ps":
						if calls > 2 {
							return nil, nil
						}
					}
					return wslList("Ubuntu-24.04\r\n"), nil
				}
				executions++
				if args[3] == "/usr/bin/id" {
					if failure == "id unavailable" {
						return nil, errors.New("id absent")
					}
					return []byte("1000\n"), nil
				}
				if failure == "ps unavailable" {
					return nil, errors.New("ps absent")
				}
				return []byte("ps\nrenderer\n"), nil
			}}
			if failure == "invalid distribution" {
				w.Distribution = "--injected"
			}
			got := w.Match(ctx, "renderer", "")
			if got.State != model.Unknown {
				t.Fatal(got)
			}
			if failure == "stopped" || failure == "missing" || failure == "unreadable" || failure == "malformed UTF16" || failure == "unpaired surrogate" || failure == "cancelled" || failure == "invalid distribution" {
				if executions != 0 {
					t.Fatal("inspected unavailable distribution")
				}
			}
			if failure == "stop before ps" && executions != 1 {
				t.Fatal("ps launched after observed stop")
			}
		})
	}
	// Model a stop after the *last* preflight: ordinary WSL launch may restart.
	executions := 0
	w := native.WSL{Distribution: "Ubuntu-24.04", Run: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "--list" {
			return []byte("Ubuntu-24.04\n"), nil
		}
		executions++
		if args[3] == "/usr/bin/id" {
			return []byte("1000\n"), nil
		}
		return []byte("ps\nrenderer\n"), nil
	}}
	if got := w.Match(context.Background(), "renderer", ""); got.State != model.Active || executions != 2 {
		t.Fatal(got)
	}
}

func TestWSLSetupHTTPHasFixedArgumentsAndPrivateStdin(t *testing.T) {
	var calls [][]string
	var input []byte
	w := native.WSL{Distribution: "Ubuntu-24.04", Run: func(_ context.Context, args []string, data []byte) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "--list" {
			return wslList("Ubuntu-24.04\r\n"), nil
		}
		input = bytes.Clone(data)
		return []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n"), nil
	}}
	carrier, err := w.HTTPTransport("http://localhost:3773/mcp")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://localhost:3773/mcp", strings.NewReader(`{"text":"$(command) quoted \"value\""}`))
	req.Header.Set("Authorization", "Bearer test-only-token")
	req.Header.Set("Content-Type", "application/json")
	response, err := carrier.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "{}" {
		t.Fatal("raw HTTP/chunk parsing", err)
	}
	want := []string{"--distribution", "Ubuntu-24.04", "--exec", "/usr/bin/curl", "-q", "--config", "-"}
	if len(calls) != 2 || !reflect.DeepEqual(calls[1], want) {
		t.Fatal(calls)
	}
	if strings.Contains(strings.Join(calls[1], " "), "test-only-token") || !bytes.Contains(input, []byte("Bearer test-only-token")) || !bytes.Contains(input, []byte("noproxy")) || !bytes.Contains(input, []byte("connect-to")) {
		t.Fatal("credential or destination handling")
	}
	for _, bad := range []struct{ url, method, header, value string }{{"http://elsewhere:3773/mcp", "POST", "", ""}, {"http://localhost:3773/mcp", "GET", "", ""}, {"http://localhost:3773/mcp", "POST", "X-Unapproved", "x"}, {"http://localhost:3773/mcp", "POST", "Authorization", "x\r\ny"}} {
		request, _ := http.NewRequest(bad.method, bad.url, nil)
		if bad.header != "" {
			request.Header[bad.header] = []string{bad.value}
		}
		before := len(calls)
		if _, err := carrier.RoundTrip(request); err == nil || len(calls) != before {
			t.Fatal("unsafe request reached WSL")
		}
	}
}

func TestWSLHTTPRejectsMalformedAndUnavailableTransport(t *testing.T) {
	for _, text := range []string{"bad response", strings.Repeat("x", (16<<10)+1) + "\r\n\r\n", "HTTP/1.1 200 OK\r\n\r\n" + strings.Repeat("x", t3.MaxBytes+(16<<10)), "HTTP/1.1 BAD\r\n\r\n"} {
		w := native.WSL{Distribution: "Ubuntu", Run: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
			if args[0] == "--list" {
				return []byte("Ubuntu\n"), nil
			}
			return []byte(text), nil
		}}
		carrier, _ := w.HTTPTransport("http://localhost:3773/mcp")
		req, _ := http.NewRequest("POST", "http://localhost:3773/mcp", nil)
		if _, err := carrier.RoundTrip(req); err == nil {
			t.Fatal("accepted malformed HTTP")
		}
	}
	w := native.WSL{Distribution: "Ubuntu", Run: func(context.Context, []string, []byte) ([]byte, error) { return nil, errors.New("unavailable") }}
	carrier, _ := w.HTTPTransport("http://localhost:3773/mcp")
	req, _ := http.NewRequest("POST", "http://localhost:3773/mcp", nil)
	if _, err := carrier.RoundTrip(req); err == nil {
		t.Fatal("unavailable WSL accepted")
	}
	if _, err := w.HTTPTransport("http://remote:3773/mcp"); err == nil {
		t.Fatal("remote endpoint accepted")
	}
}

func TestNativeT3OwnerAndIdentityCollision(t *testing.T) {
	requireNative(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "native T3 protocol fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "t3_environment_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"environmentId": "native-fixture", "platform": map[string]any{"os": "windows"}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer httpServer.Close()
	b := t3.Binding{Endpoint: httpServer.URL + "/mcp", EnvironmentID: "native-fixture"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := native.VerifyT3(ctx, b, "fixture-token"); err != nil {
		t.Fatal("native current-user identity", err)
	}
	b.EnvironmentID = "wrong-environment"
	if native.VerifyT3(ctx, b, "fixture-token") == nil {
		t.Fatal("identity silently rebound")
	}
	b.EnvironmentID = "native-fixture"
	b.WSLDistribution = "Ubuntu-24.04"
	if native.VerifyT3Owner(ctx, b) == nil {
		t.Fatal("native listener accepted as WSL")
	}
	httpServer.Close()
	b.WSLDistribution = ""
	if native.VerifyT3Owner(ctx, b) == nil {
		t.Fatal("closed endpoint accepted")
	}
}

func TestNativeT3ProtectedBindings(t *testing.T) {
	requireNative(t)
	dir := filepath.Join(t.TempDir(), "private-bindings")
	if data, err := native.LoadT3Bindings(dir); err != nil || len(data) != 0 {
		t.Fatal("missing registry", err)
	}
	for _, data := range [][]byte{[]byte(`{"schema_version":1,"bindings":[]}`), []byte(`{"schema_version":1,"bindings":[{"id":"fixture"}]}`)} {
		if err := native.SaveT3Bindings(dir, data); err != nil {
			t.Fatal(err)
		}
		got, err := native.LoadT3Bindings(dir)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("atomic registry roundtrip", err)
		}
	}
	if err := native.SaveT3Bindings(dir, make([]byte, t3.MaxBytes+1)); err == nil {
		t.Fatal("oversized binding write")
	}
	name := filepath.Join(dir, "environments.json")
	if err := os.WriteFile(name, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := native.LoadT3Bindings(dir); err == nil {
		t.Fatal("empty corrupt registry treated as absent")
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if native.SaveT3Bindings(dir, []byte("{}")) == nil {
		t.Fatal("directory used as registry")
	}
	if _, err := native.LoadT3Bindings(dir); err == nil {
		t.Fatal("directory read as registry")
	}
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, []byte("x"), 0600)
	if native.SaveT3Bindings(file, []byte("{}")) == nil {
		t.Fatal("file used as registry directory")
	}
}

func TestNativeIPv6T3ListenerOwner(t *testing.T) {
	requireNative(t)
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal("IPv6 loopback fixture", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	b := t3.Binding{Endpoint: server.URL + "/mcp", EnvironmentID: "native-fixture"}
	if err := native.VerifyT3Owner(context.Background(), b); err != nil {
		t.Fatal("IPv6 current-user owner", err)
	}
	client, err := t3.HTTPClient(context.Background(), b.Endpoint, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(b.Endpoint)
	if err != nil {
		t.Fatal("IPv6 pinned local transport", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal(response.StatusCode)
	}
}

func t3Registry(dir string) *t3.Registry {
	return &t3.Registry{Read: func() ([]byte, error) { return native.LoadT3Bindings(dir) }, Write: func(data []byte) error { return native.SaveT3Bindings(dir, data) }, Secrets: native.NewCredentials(), Owner: native.VerifyT3Owner, Verify: native.VerifyT3}
}

func TestNativeSelectedWSLSetupAndConsoleObservation(t *testing.T) {
	requireNative(t)
	endpoint := os.Getenv("SNOOZAP_WSL_FIXTURE_URL")
	distribution := os.Getenv("SNOOZAP_WSL_DISTRIBUTION")
	if endpoint == "" || distribution == "" {
		t.Skip("native fixture driver supplies selected WSL endpoint and distribution")
	}
	dir := filepath.Join(t.TempDir(), "private-t3")
	registry := t3Registry(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	binding, err := registry.Setup(ctx, t3.Selection{Endpoint: endpoint + "/mcp", WSLDistribution: distribution}, func(ctx context.Context, raw string) error {
		request, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
		if e != nil {
			return e
		}
		response, e := http.DefaultClient.Do(request)
		if e != nil {
			return e
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("fixture approval failed")
		}
		return nil
	})
	if err != nil {
		t.Fatal("selected WSL setup", err)
	}
	defer func() {
		if err := native.NewCredentials().Delete(context.Background(), binding.CredentialRef); err != nil {
			t.Error("fixture credential cleanup", err)
		}
	}()
	if binding.EnvironmentID != "local-environment" {
		t.Fatal("wrong fixture identity")
	}
	if got := (t3.Probe{Registry: registry, Environment: binding.ID}).Check(ctx); got.State != model.Active {
		t.Fatal("selected WSL observation", got)
	}
	wrong := binding
	wrong.WSLDistribution = "SnooZap-Missing-Distribution-601v"
	if native.VerifyT3(ctx, wrong, "fixture-token-never-include-in-diagnostics") == nil {
		t.Fatal("missing distribution accepted")
	}
	wrong = binding
	wrong.WSLDistribution = ""
	if native.VerifyT3Owner(ctx, wrong) == nil {
		t.Fatal("WSL relay accepted as native")
	}
	data, _ := json.Marshal(map[string]any{"schema_version": 1, "probes": map[string]any{"t3": map[string]any{"type": "t3", "local_environment": binding.ID, "interval_ms": 500, "timeout_ms": 400}}, "rules": []any{map[string]any{"id": "work", "when": map[string]any{"probe": "t3"}}}})
	if err := native.SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	owner := launch(t, executable(t), dir)
	applicationStatus(t, owner, func(s model.Snapshot) bool { return s.Activity == model.Active && s.NativeRequestAccepted })
	owner.stop(t)
}

func TestNativeWSLRunningAndMissingProcess(t *testing.T) {
	requireNative(t)
	distribution := os.Getenv("SNOOZAP_WSL_DISTRIBUTION")
	if distribution == "" {
		t.Skip("native driver supplies already-running WSL distribution")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := native.WSL{Distribution: distribution}
	if got := w.Match(ctx, "snoozap-no-such-app-601v", ""); got.State != model.Inactive {
		t.Fatal("complete current-user process inventory", got)
	}
	if got := w.Match(ctx, "", "/snoozap-no-such-app-601v"); got.State != model.Inactive && !(got.State == model.Unknown && got.ErrorCode == "incomplete") {
		t.Fatal("unsafe executable observation", got)
	} else if got.State == model.Unknown {
		t.Log("Ambient executable inventory is unreadable; unknown is required, not confirmed idle")
	}
	fixture := os.Getenv("SNOOZAP_WSL_FIXTURE_EXE")
	if fixture == "" {
		t.Fatal("driver must provide the owned running WSL fixture executable")
	}
	if got := w.Match(ctx, "", fixture); got.State != model.Active {
		t.Fatal("known running current-user executable", got)
	}
	w.Distribution = "SnooZap-Missing-Distribution-601v"
	if got := w.Match(ctx, "app", ""); got.State != model.Unknown {
		t.Fatal("missing distribution became idle", got)
	}
}

func TestNativeLiveT3Setup(t *testing.T) {
	requireNative(t)
	endpoint := os.Getenv("SNOOZAP_LIVE_T3_ENDPOINT")
	if endpoint == "" {
		t.Skip("explicit separately authorized live product setup driver only")
	}
	dir := filepath.Join(t.TempDir(), "private-live-t3")
	registry := t3Registry(dir)
	distribution := os.Getenv("SNOOZAP_WSL_DISTRIBUTION")
	binding, err := registry.Setup(context.Background(), t3.Selection{Endpoint: endpoint, WSLDistribution: distribution}, func(ctx context.Context, raw string) error {
		command := exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", raw)
		if err := command.Start(); err != nil {
			return err
		}
		go func() { _ = command.Wait() }()
		return nil
	})
	if err != nil {
		t.Fatal("live product setup", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			if err := native.NewCredentials().Delete(context.Background(), binding.CredentialRef); err != nil {
				t.Error("live credential cleanup", err)
			}
		}
	}()
	if binding.EnvironmentID != os.Getenv("SNOOZAP_LIVE_T3_ENVIRONMENT") {
		t.Fatal("live environment identity mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observation := (t3.Probe{Registry: registry, Environment: binding.ID}).Check(ctx)
	if observation.State != model.Active {
		t.Fatal("live busy metadata", observation)
	}
	if err := registry.RemoveUnused(context.Background(), binding.ID, nil); err != nil {
		t.Fatal("live binding and credential cleanup", err)
	}
	cleaned = true
	if _, err := native.NewCredentials().Read(context.Background(), binding.CredentialRef); err == nil {
		t.Fatal("live credential remained after deletion")
	}
	_, _ = fmt.Fprintf(os.Stdout, "LIVE_PRODUCT_T3_PASS environment=%s state=%s private_registry=true os_credentials=true namespace_verified=true os_token_deleted=true\n", binding.EnvironmentID, observation.State)
}

func TestNativeWSLProcessComposition(t *testing.T) {
	requireNative(t)
	distribution := os.Getenv("SNOOZAP_WSL_DISTRIBUTION")
	fixture := os.Getenv("SNOOZAP_WSL_FIXTURE_EXE")
	if distribution == "" || fixture == "" {
		t.Skip("native driver provides owned running WSL fixture")
	}
	dir := filepath.Join(t.TempDir(), "private-wsl-process")
	data, _ := json.Marshal(map[string]any{"schema_version": 1, "probes": map[string]any{"work": map[string]any{"type": "process", "executable": fixture, "wsl_distribution": distribution, "interval_ms": 1000, "timeout_ms": 900}}, "rules": []any{map[string]any{"id": "work", "when": map[string]any{"probe": "work"}}}})
	if err := native.SaveConfig(dir, data); err != nil {
		t.Fatal(err)
	}
	owner := launch(t, executable(t), dir)
	applicationStatus(t, owner, func(s model.Snapshot) bool { return s.Activity == model.Active && s.NativeRequestAccepted })
	owner.stop(t)
}
