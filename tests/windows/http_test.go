//go:build windows

package windows_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
	native "github.com/omry/snoozap/internal/platform/windows"
	"github.com/omry/snoozap/internal/probe"
	"github.com/omry/snoozap/internal/probe/t3"
)

func TestNativeHTTPConsoleIPCConfigCredentialsAndHistory(t *testing.T) {
	requireNative(t)
	const secret = "SnooZap-owned-HTTP-fixture-header"
	ref := fmt.Sprintf("http-fixture-%d", time.Now().UnixNano())
	credentials := native.NewCredentials()
	if err := credentials.Write(context.Background(), ref, []byte(secret)); err != nil {
		t.Fatal("fixture credential write", err)
	}
	t.Cleanup(func() {
		if err := credentials.Delete(context.Background(), ref); err != nil {
			t.Error("fixture credential cleanup", err)
		}
	})
	var response atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != secret {
			t.Error("actual console did not load OS credential")
			w.WriteHeader(401)
			return
		}
		switch response.Load() {
		case 0:
			io.WriteString(w, `{"work":{"active":true}}`)
		case 1:
			io.WriteString(w, `{"work":{"active":false}}`)
		case 2:
			io.WriteString(w, `{"work":{"active":"private-body"}}`)
		case 3:
			w.WriteHeader(503)
			io.WriteString(w, "private-body")
		case 4:
			io.WriteString(w, strings.Repeat("x", (1<<20)+1))
		}
	}))
	defer server.Close()
	input := fmt.Sprintf(`{"schema_version":1,"probes":{"web":{"type":"http","url":%q,"pointer":"/work/active","interval_ms":200,"timeout_ms":150,"credential_headers":{"Authorization":%q}}},"rules":[{"id":"work","when":{"probe":"web"}}]}`, server.URL+"?private-query", ref)
	cfg, err := config.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "http-config")
	if err := native.SaveConfig(dir, []byte(input)); err != nil {
		t.Fatal(err)
	}
	owner := launch(t, executable(t), dir)
	applicationStatus(t, owner, func(s model.Snapshot) bool {
		return s.Activity == model.Active && s.NativeRequestAccepted && s.Hold.Requested
	})
	client := ipc.NewClient(ipc.Dial)
	defer client.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	wait := func(state model.State, hold bool) {
		t.Helper()
		applicationStatus(t, owner, func(s model.Snapshot) bool {
			return len(s.Probes) == 1 && s.Probes[0].State == state && s.Hold.Requested == hold
		})
		s, err := client.Snapshot(ctx)
		if err != nil || len(s.Probes) != 1 || s.Probes[0].State != state || s.Hold.Requested != hold {
			t.Fatal("IPC disagrees with console", err, s)
		}
	}
	response.Store(2)
	wait(model.Unknown, true) // Retain the last confirmed active hold on errors.
	response.Store(1)
	wait(model.Inactive, false)
	for _, mode := range []int32{3, 4} {
		response.Store(mode)
		// Wait for a fresh poll of this response, even if the public unknown
		// state is unchanged from the preceding failure category.
		before, err := client.History(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last := uint64(0)
		for _, r := range before.Records {
			if r.ProbeID == "web" && r.Operation == "probe" {
				last = r.Sequence
			}
		}
		deadline := time.Now().Add(2 * time.Second)
		seen := false
		for time.Now().Before(deadline) {
			h, err := client.History(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range h.Records {
				if r.ProbeID == "web" && r.Operation == "probe" && r.Sequence > last && (mode == 3 && r.StatusCode == 503 || mode == 4 && r.StatusCode == 200) {
					seen = true
				}
			}
			if seen {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !seen {
			t.Fatal("fresh failure poll missing", mode)
		}
		wait(model.Unknown, false)
	}
	response.Store(0)
	wait(model.Active, true)
	bad := config.Clone(cfg)
	p := bad.Probes["web"]
	p.URL = "http://192.168.56.1/"
	bad.Probes["web"] = p
	if _, err := client.Execute(ctx, model.Command{Operation: "config", Configuration: &bad}); err == nil {
		t.Fatal("IPC accepted nonloopback HTTP candidate")
	}
	data, err := client.ExportConfig(ctx)
	if err != nil || bytes.Contains(data, []byte(secret)) {
		t.Fatal("credential value exported", err)
	}
	if _, err := config.Parse(bytes.NewReader(data)); err != nil {
		t.Fatal("export not portable", err)
	}
	// An accepted replacement uses the same owner, scheduler and atomic store.
	p = cfg.Probes["web"]
	p.Operator = "exists"
	p.Pointer = "/missing"
	cfg.Probes["web"] = p
	if _, err := client.Execute(ctx, model.Command{Operation: "config", Configuration: &cfg}); err != nil {
		t.Fatal("IPC HTTP config apply", err)
	}
	wait(model.Inactive, false)
	saved, err := native.LoadConfig(dir)
	if err != nil || saved.Probes["web"].Pointer != "/missing" {
		t.Fatal("HTTP candidate not persisted", err)
	}
	h, err := client.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(h)
	for _, private := range []string{secret, "private-body", "private-query", server.URL} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("private HTTP material in history")
		}
	}
	seen := map[string]bool{}
	for _, r := range h.Records {
		if r.Operation == "probe" && r.ProbeID == "web" {
			seen[fmt.Sprintf("%s/%d", r.Result, r.StatusCode)] = true
		}
	}
	for _, expected := range []string{"active/200", "inactive/200", "unavailable/200", "unavailable/503"} {
		if !seen[expected] {
			t.Fatal("missing HTTP history category", expected)
		}
	}
	if requests.Load() < 6 {
		t.Fatal("missing actual HTTP polls")
	}
	owner.stop(t)
	if err := credentials.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if value, err := credentials.Read(context.Background(), ref); err == nil {
		clear(value)
		t.Fatal("owned fixture credential survived deletion")
	}
}

func TestNativePowerProbeUsesSystemSource(t *testing.T) {
	requireNative(t)
	reader := native.SystemPower{}
	b, err := reader.ReadPower(context.Background())
	registry := probe.Registry{Power: reader}
	if err != nil || !b.Known || (b.Source != "ac" && b.Source != "battery") {
		if registry.Resolve(config.Probe{Type: "power", Source: "ac"}).Check(context.Background()).State != model.Unknown {
			t.Fatal("unknown system power reported as known")
		}
		return
	}
	for _, source := range []string{"ac", "battery"} {
		expected := model.Inactive
		if b.Source == source {
			expected = model.Active
		}
		if got := registry.Resolve(config.Probe{Type: "power", Source: source}).Check(context.Background()); got.State != expected {
			t.Fatal("native source comparison", got, b)
		}
	}
}

func TestNativeMixedHTTPPowerProcessAndT3Registry(t *testing.T) {
	requireNative(t)
	var webActive, t3Active, webFailure atomic.Bool
	webActive.Store(true)
	t3Active.Store(true)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if webFailure.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, webActive.Load())
	}))
	defer web.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "SnooZap mixed-rule protocol fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "t3_environment_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"environmentId": "mixed-fixture", "platform": map[string]any{"os": "windows"}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "t3_thread_list", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rows := []any{}
		if t3Active.Load() {
			rows = append(rows, map[string]any{"threadId": "owned-fixture-thread", "status": "running", "settled": false, "environmentId": "mixed-fixture"})
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"threads": rows, "nextCursor": nil, "total": len(rows)}}, nil
	})
	const token = "owned-mixed-rule-test-token"
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer mcpServer.Close()
	dir := filepath.Join(t.TempDir(), "mixed-config")
	ref := fmt.Sprintf("mixed-fixture-%d", time.Now().UnixNano())
	credentials := native.NewCredentials()
	// This controlled fixture seeds its own credential and binding. It tests
	// authenticated polling, native owner checks and composition, not OAuth.
	secret, _ := json.Marshal(map[string]any{"access_token": token, "expiry": time.Now().Add(time.Hour), "scope": "orchestration:read"})
	if err := credentials.Write(context.Background(), ref, secret); err != nil {
		clear(secret)
		t.Fatal(err)
	}
	clear(secret)
	t.Cleanup(func() {
		if err := credentials.Delete(context.Background(), ref); err != nil {
			t.Error(err)
		}
	})
	binding := t3.Binding{ID: "mixed", Endpoint: mcpServer.URL + "/mcp", EnvironmentID: "mixed-fixture", CredentialRef: ref}
	bindings, _ := json.Marshal(map[string]any{"schema_version": 1, "bindings": []t3.Binding{binding}})
	if err := native.SaveT3Bindings(dir, bindings); err != nil {
		t.Fatal(err)
	}
	b, err := (native.SystemPower{}).ReadPower(context.Background())
	if err != nil || !b.Known || b.Source != "ac" && b.Source != "battery" {
		t.Fatal("mixed native test requires a known system source", err, b)
	}
	input := fmt.Sprintf(`{"schema_version":1,"probes":{"web":{"type":"http","url":%q,"interval_ms":300,"timeout_ms":250},"app":{"type":"process","executable":%q,"interval_ms":300,"timeout_ms":250},"power":{"type":"power","source":%q,"interval_ms":300,"timeout_ms":250},"t3":{"type":"t3","local_environment":"mixed","interval_ms":300,"timeout_ms":250}},"rules":[{"id":"mixed","when":{"all":[{"probe":"web"},{"probe":"app"},{"probe":"power"},{"probe":"t3"}]}}]}`, web.URL, executable(t), b.Source)
	if err := native.SaveConfig(dir, []byte(input)); err != nil {
		t.Fatal(err)
	}
	owner := launch(t, executable(t), dir)
	wait := func(state model.State, hold bool) {
		t.Helper()
		applicationStatus(t, owner, func(s model.Snapshot) bool {
			if len(s.Probes) != 4 || s.Activity != state || s.Hold.Requested != hold || hold && !s.Hold.OSAccepted {
				return false
			}
			for _, p := range s.Probes {
				if (p.ID == "app" || p.ID == "power") && p.State != model.Active {
					return false
				}
			}
			return true
		})
	}
	wait(model.Active, true)
	webActive.Store(false)
	wait(model.Inactive, false)
	t3Active.Store(false)
	webActive.Store(true)
	wait(model.Inactive, false)
	// Require a confirmed inactive T3 observation before restoring it.
	applicationStatus(t, owner, func(s model.Snapshot) bool {
		for _, p := range s.Probes {
			if p.ID == "t3" {
				return p.State == model.Inactive
			}
		}
		return false
	})
	t3Active.Store(true)
	wait(model.Active, true)
	webFailure.Store(true)
	wait(model.Unknown, true)
	owner.stop(t)
}
