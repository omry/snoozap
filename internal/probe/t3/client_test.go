package t3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omry/snoozap/internal/model"
)

const fixtureToken = "fixture-token-never-include-in-diagnostics"

type fixture struct {
	server              *httptest.Server
	url                 string
	identity            any
	page                func(map[string]json.RawMessage) any
	isError             bool
	textOnly            bool
	scope               string
	revoked             atomic.Bool
	opens               atomic.Int32
	mu                  sync.Mutex
	args                []map[string]json.RawMessage
	challenge, redirect string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{identity: map[string]any{"environmentId": "local-environment", "platform": map[string]any{"os": "windows"}}, scope: "orchestration:read"}
	f.page = func(map[string]json.RawMessage) any { return page([]any{}, nil, 0) }
	server := mcp.NewServer(&mcp.Implementation{Name: "T3 contract fixture", Version: "0.1.8-nightly.20261104.4179"}, nil)
	server.AddTool(&mcp.Tool{Name: "t3_environment_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: f.identity}, nil
	})
	server.AddTool(&mcp.Tool{Name: "t3_thread_list", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]json.RawMessage
		if json.Unmarshal(req.Params.Arguments, &args) != nil {
			return nil, ErrSchema
		}
		f.mu.Lock()
		f.args = append(f.args, args)
		f.mu.Unlock()
		value := f.page(args)
		if f.textOnly {
			data, _ := json.Marshal(value)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, IsError: f.isError}, nil
		}
		return &mcp.CallToolResult{StructuredContent: value, IsError: f.isError}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			write(map[string]any{"resource": f.url + "/mcp", "authorization_servers": []string{f.url}, "scopes_supported": []string{"orchestration:read", "orchestration:operate"}, "bearer_methods_supported": []string{"header"}})
		case "/.well-known/oauth-authorization-server":
			write(map[string]any{"issuer": f.url, "authorization_endpoint": f.url + "/authorize", "token_endpoint": f.url + "/token", "registration_endpoint": f.url + "/register", "scopes_supported": []string{"orchestration:read", "orchestration:operate"}, "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
		case "/register":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			body["client_id"] = "fixture-client"
			write(body)
		case "/authorize":
			q := r.URL.Query()
			if q.Get("scope") != "orchestration:read" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
				http.Error(w, "invalid authorization", 400)
				return
			}
			f.mu.Lock()
			f.challenge = q.Get("code_challenge")
			f.redirect = q.Get("redirect_uri")
			f.mu.Unlock()
			callback, err := url.Parse(q.Get("redirect_uri"))
			if err != nil {
				http.Error(w, "invalid callback", 400)
				return
			}
			values := callback.Query()
			values.Set("code", "fixture-code")
			values.Set("state", q.Get("state"))
			callback.RawQuery = values.Encode()
			http.Redirect(w, r, callback.String(), http.StatusFound)
		case "/token":
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			f.mu.Lock()
			challenge, redirect := f.challenge, f.redirect
			f.mu.Unlock()
			if r.Form.Get("code") != "fixture-code" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "fixture-client" || r.Form.Get("redirect_uri") != redirect || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				http.Error(w, "invalid_grant", 400)
				return
			}
			write(map[string]any{"access_token": fixtureToken, "token_type": "Bearer", "expires_in": 3600, "scope": f.scope})
		case "/mcp":
			if f.revoked.Load() || r.Header.Get("Authorization") != "Bearer "+fixtureToken {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.url+`/.well-known/oauth-protected-resource/mcp"`)
				http.Error(w, "sensitive server detail must not escape", 401)
				return
			}
			handler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	f.url = f.server.URL
	t.Cleanup(f.server.Close)
	return f
}

func page(rows []any, next any, total any) map[string]any {
	return map[string]any{"projectId": nil, "currentThreadId": nil, "threads": rows, "nextCursor": next, "total": total}
}
func row(id, status string, settled bool) map[string]any {
	return map[string]any{"threadId": id, "status": status, "settled": settled, "projectId": "project"}
}
func (f *fixture) open(ctx context.Context, raw string) error {
	f.opens.Add(1)
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	q := u.Query()
	if q.Get("scope") != "orchestration:read" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != f.url+"/mcp" {
		return ErrAuthorization
	}
	request, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		return ErrAuthorization
	}
	return nil
}

type memorySecrets struct {
	mu                           sync.Mutex
	values                       map[string][]byte
	writeErr, readErr, deleteErr error
	deleted                      []string
	writes                       int
}

func (m *memorySecrets) Read(ctx context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	return bytes.Clone(m.values[key]), ctx.Err()
}
func (m *memorySecrets) Write(ctx context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.writeErr != nil {
		return m.writeErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.values[key] = bytes.Clone(data)
	return nil
}
func (m *memorySecrets) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, key)
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.values, key)
	return ctx.Err()
}

type memoryRegistry struct {
	mu                sync.Mutex
	data              []byte
	readErr, writeErr error
}

func (m *memoryRegistry) read() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bytes.Clone(m.data), m.readErr
}
func (m *memoryRegistry) write(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	m.data = bytes.Clone(data)
	return nil
}
func registry(f *fixture) (*Registry, *memoryRegistry, *memorySecrets, Binding) {
	b := Binding{ID: "local", Endpoint: f.url + "/mcp", EnvironmentID: "local-environment", CredentialRef: "test_ref"}
	data, _ := json.Marshal(registryDocument{SchemaVersion: 1, Bindings: []Binding{b}})
	store := &memoryRegistry{data: data}
	secret, _ := json.Marshal(credential{AccessToken: fixtureToken, Scope: "orchestration:read", Expiry: time.Now().Add(time.Hour)})
	secrets := &memorySecrets{values: map[string][]byte{b.CredentialRef: secret}}
	r := &Registry{Read: store.read, Write: store.write, Secrets: secrets, Owner: func(context.Context, Binding) error { return nil }, Verify: func(ctx context.Context, b Binding, token string) error { return VerifyIdentity(ctx, b, token, nil) }}
	return r, store, secrets, b
}

func TestThreadContractActivityAndCompleteness(t *testing.T) {
	tests := []struct {
		name  string
		pages []any
		state model.State
		code  string
	}{
		{"empty", []any{page([]any{}, nil, 0)}, model.Inactive, ""},
		{"child waiting", []any{page([]any{row("child", "waiting", false)}, nil, 1)}, model.Active, ""},
		{"settled excluded", []any{page([]any{row("done", "running", true)}, nil, 1)}, model.Inactive, ""},
		{"settled status", []any{page([]any{row("done", "completed", false)}, nil, 1)}, model.Inactive, ""},
		{"page then busy", []any{page([]any{row("idle", "idle", false)}, 1, 2), page([]any{row("busy", "queued", false)}, nil, 2)}, model.Active, ""},
		{"complete inactive pages", []any{page([]any{row("a", "idle", false)}, 1, 2), page([]any{row("b", "completed", true)}, nil, 2)}, model.Inactive, ""},
		{"truncated", []any{page([]any{}, nil, 1)}, model.Unknown, "incomplete"},
		{"duplicate", []any{page([]any{row("a", "idle", false)}, 1, 2), page([]any{row("a", "idle", false)}, nil, 2)}, model.Unknown, "incomplete"},
		{"moving total", []any{page([]any{row("a", "idle", false)}, 1, 2), page([]any{}, nil, 1)}, model.Unknown, "incomplete"},
		{"cursor cycle", []any{page([]any{}, 0, 2), page([]any{}, 0, 2)}, model.Unknown, "incomplete"},
		{"negative cursor", []any{page([]any{}, -1, 2)}, model.Unknown, "incomplete"},
		{"unknown status", []any{page([]any{row("a", "new-schema", false)}, nil, 1)}, model.Unknown, "schema"},
		{"null total", []any{page([]any{}, nil, nil)}, model.Unknown, "schema"},
		{"null rows", []any{page(nil, nil, 0)}, model.Unknown, "schema"},
		{"missing cursor", []any{map[string]any{"threads": []any{}, "total": 0}}, model.Unknown, "schema"},
		{"missing settled", []any{page([]any{map[string]any{"threadId": "a", "status": "running"}}, nil, 1)}, model.Unknown, "schema"},
		{"foreign excluded", []any{page([]any{map[string]any{"threadId": "remote", "status": "running", "settled": false, "environmentId": "foreign"}}, nil, 1)}, model.Inactive, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			i := 0
			f.page = func(map[string]json.RawMessage) any {
				if i >= len(test.pages) {
					return nil
				}
				result := test.pages[i]
				i++
				return result
			}
			r, _, _, _ := registry(f)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got := (Probe{Registry: r, Environment: "local"}).Check(ctx)
			if got.State != test.state || got.ErrorCode != test.code {
				t.Fatalf("got%+v want%s/%s", got, test.state, test.code)
			}
			if f.opens.Load() != 0 {
				t.Fatal("polling opened authorization")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, args := range f.args {
				if string(args["allProjects"]) != "true" || string(args["includeSubagents"]) != "true" || string(args["settled"]) != "false" || string(args["limit"]) != "100" {
					t.Fatalf("incorrect query: %s", args)
				}
				var statuses []string
				_ = json.Unmarshal(args["statuses"], &statuses)
				if !reflect.DeepEqual(statuses, []string{"preparing", "queued", "starting", "running", "waiting"}) {
					t.Fatal(statuses)
				}
				if _, ok := args["environmentId"]; ok {
					t.Fatal("routed to another saved environment")
				}
			}
		})
	}
}

func TestAllBusyStatesAndTextOnlyContract(t *testing.T) {
	for _, status := range []string{"preparing", "queued", "starting", "running", "waiting"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			f.textOnly = true
			f.page = func(map[string]json.RawMessage) any { return page([]any{row("work", status, false)}, nil, 1) }
			r, _, _, _ := registry(f)
			if got := (Probe{Registry: r, Environment: "local"}).Check(context.Background()); got.State != model.Active {
				t.Fatal(got)
			}
		})
	}
}

func TestPollingFailsSafelyWithoutReauthorization(t *testing.T) {
	for _, failure := range []string{"missing binding", "bad registry", "registry read", "credential read", "credential malformed", "expired", "strong scope", "owner", "identity", "platform", "revoked", "tool error", "timeout", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			r, store, secrets, b := registry(f)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch failure {
			case "missing binding":
				b.ID = "absent"
			case "bad registry":
				store.data = []byte(`{"schema_version":99}`)
			case "registry read":
				store.readErr = errors.New(fixtureToken)
			case "credential read":
				secrets.readErr = errors.New(fixtureToken)
			case "credential malformed":
				secrets.values[b.CredentialRef] = []byte("bad")
			case "expired":
				secrets.values[b.CredentialRef], _ = json.Marshal(credential{AccessToken: fixtureToken, Scope: "orchestration:read", Expiry: time.Now().Add(-time.Second)})
			case "strong scope":
				secrets.values[b.CredentialRef], _ = json.Marshal(credential{AccessToken: fixtureToken, Scope: "orchestration:operate", Expiry: time.Now().Add(time.Hour)})
			case "owner":
				r.Owner = func(context.Context, Binding) error { return errors.New(fixtureToken) }
			case "identity":
				f.identity = map[string]any{"environmentId": "foreign", "platform": map[string]any{"os": "windows"}}
			case "platform":
				f.identity = map[string]any{"environmentId": "local-environment", "platform": map[string]any{"os": "linux"}}
			case "revoked":
				f.revoked.Store(true)
			case "tool error":
				f.isError = true
			case "timeout":
				ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
				defer cancel()
				<-ctx.Done()
			case "cancelled":
				cancel()
			}
			got := (Probe{Registry: r, Environment: b.ID}).Check(ctx)
			if got.State != model.Unknown || got.ErrorCode == "" || strings.Contains(fmt.Sprint(got), fixtureToken) {
				t.Fatal(got)
			}
			if f.opens.Load() != 0 {
				t.Fatal("failure requested another sign-in")
			}
		})
	}
	if got := (Probe{}).Check(context.Background()); got.State != model.Unknown {
		t.Fatal(got)
	}
}

func TestPageBudgetAndMalformedSchema(t *testing.T) {
	f := newFixture(t)
	f.page = func(args map[string]json.RawMessage) any {
		var cursor int
		_ = json.Unmarshal(args["cursor"], &cursor)
		return page([]any{row(fmt.Sprint(cursor), "idle", false)}, cursor+1, 9999)
	}
	r, _, _, _ := registry(f)
	if got := (Probe{Registry: r, Environment: "local"}).Check(context.Background()); got.State != model.Unknown || got.ErrorCode != "incomplete" {
		t.Fatal(got)
	}
	f.mu.Lock()
	calls := len(f.args)
	f.mu.Unlock()
	if calls != 32 {
		t.Fatal(calls)
	}
}

func TestSetupUsesPKCEReadOnlyAndPersistsAfterVerification(t *testing.T) {
	f := newFixture(t)
	r, store, secrets, _ := registry(f)
	store.data = nil
	var verified atomic.Bool
	r.Verify = func(ctx context.Context, b Binding, token string) error {
		if token != fixtureToken {
			t.Fatal("wrong token")
		}
		if secrets.writes != 0 {
			t.Fatal("persisted before identity")
		}
		verified.Store(true)
		return VerifyIdentity(ctx, b, token, nil)
	}
	b, err := r.Setup(context.Background(), Selection{Endpoint: f.url + "/mcp"}, f.open)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Load() || !Identifier(b.ID) || b.CredentialRef == "" || f.opens.Load() != 1 {
		t.Fatal(b)
	}
	if bytes.Contains(store.data, []byte(fixtureToken)) || bytes.Contains(store.data, []byte("access_token")) {
		t.Fatal("secret in binding registry")
	}
	if len(secrets.values) != 2 || secrets.writes != 1 || len(secrets.deleted) != 0 {
		t.Fatal("incorrect credential lifetime")
	}
	if got := (Probe{Registry: r, Environment: b.ID}).Check(context.Background()); got.State != model.Inactive {
		t.Fatal(got)
	}
	if f.opens.Load() != 1 {
		t.Fatal("polling reopened setup")
	}
}

func TestSetupFailurePreservesRegistryAndCleansOnlyOwnCredential(t *testing.T) {
	for _, failure := range []string{"owner", "browser", "verification", "wrong identity", "strong scope", "secret write", "registry write", "cleanup", "full"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			r, store, secrets, b := registry(f)
			before := bytes.Clone(store.data)
			beforeSecret := bytes.Clone(secrets.values[b.CredentialRef])
			open := f.open
			switch failure {
			case "owner":
				r.Owner = func(context.Context, Binding) error { return ErrIdentity }
			case "browser":
				open = func(context.Context, string) error { return errors.New(fixtureToken) }
			case "verification":
				r.Verify = func(context.Context, Binding, string) error { return ErrIdentity }
			case "wrong identity":
				f.identity = map[string]any{"environmentId": "bad", "platform": map[string]any{"os": "linux"}}
			case "strong scope":
				f.scope = "orchestration:read orchestration:operate"
			case "secret write":
				secrets.writeErr = errors.New(fixtureToken)
			case "registry write":
				store.writeErr = errors.New(fixtureToken)
			case "cleanup":
				store.writeErr = errors.New(fixtureToken)
				secrets.deleteErr = errors.New(fixtureToken)
			case "full":
				var bindings []Binding
				for i := 0; i < 256; i++ {
					copy := b
					copy.ID = fmt.Sprintf("env%d", i)
					copy.CredentialRef = fmt.Sprintf("ref%d", i)
					bindings = append(bindings, copy)
				}
				store.data, _ = json.Marshal(registryDocument{SchemaVersion: 1, Bindings: bindings})
				before = bytes.Clone(store.data)
			}
			_, err := r.Setup(context.Background(), Selection{Endpoint: f.url + "/mcp"}, open)
			if err == nil || strings.Contains(err.Error(), fixtureToken) {
				t.Fatalf("unsafe result %v", err)
			}
			if !bytes.Equal(store.data, before) {
				t.Fatal("changed active registry on failure")
			}
			if !bytes.Equal(secrets.values[b.CredentialRef], beforeSecret) {
				t.Fatal("touched unrelated secret")
			}
			if failure == "cleanup" && !errors.Is(err, ErrCredentialCleanup) {
				t.Fatal(err)
			}
			if failure == "secret write" || failure == "registry write" || failure == "cleanup" {
				if len(secrets.deleted) != 1 || secrets.deleted[0] == b.CredentialRef {
					t.Fatal("did not clean exactly own temporary credential")
				}
			}
		})
	}
}

func TestSetupCancelledAndInvalidCallbackCloseListener(t *testing.T) {
	f := newFixture(t)
	r, store, _, _ := registry(f)
	before := bytes.Clone(store.data)
	ctx, cancel := context.WithCancel(context.Background())
	var callback string
	_, err := r.Setup(ctx, Selection{Endpoint: f.url + "/mcp"}, func(ctx context.Context, raw string) error {
		u, _ := url.Parse(raw)
		callback = u.Query().Get("redirect_uri")
		response, e := http.Get(callback + "?state=wrong&code=fixture-code")
		if e != nil {
			return e
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal(response.StatusCode)
		}
		cancel()
		return nil
	})
	if err == nil || !bytes.Equal(store.data, before) {
		t.Fatal("cancelled setup changed state")
	}
	client := &http.Client{Timeout: 100 * time.Millisecond}
	response, e := client.Get(callback)
	if e == nil {
		response.Body.Close()
		t.Fatal("callback survived cancelled setup")
	}
}

func TestRemoveUnusedPreservesSharedReferencesAndSupportsRetry(t *testing.T) {
	f := newFixture(t)
	r, store, secrets, original := registry(f)
	b, err := r.Setup(context.Background(), Selection{Endpoint: f.url + "/mcp"}, f.open)
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(store.data)
	secret := bytes.Clone(secrets.values[original.CredentialRef])
	if err := r.RemoveUnused(context.Background(), b.ID, []string{original.ID, b.ID}); !errors.Is(err, ErrBindingInUse) {
		t.Fatal("shared reference removed", err)
	}
	if !bytes.Equal(before, store.data) || len(secrets.deleted) != 0 {
		t.Fatal("in-use binding changed")
	}
	secrets.deleteErr = errors.New("sensitive-delete-detail")
	if err := r.RemoveUnused(context.Background(), b.ID, []string{original.ID}); !errors.Is(err, ErrCredentialCleanup) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, store.data) {
		t.Fatal("failed secret removal discarded retry reference")
	}
	secrets.deleteErr = nil
	store.writeErr = errors.New("sensitive-write-detail")
	if err := r.RemoveUnused(context.Background(), b.ID, []string{original.ID}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, store.data) {
		t.Fatal("failed registry write changed document")
	}
	store.writeErr = nil
	if err := r.RemoveUnused(context.Background(), b.ID, []string{original.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.binding(b.ID); err == nil {
		t.Fatal("removed binding still exists")
	}
	if _, ok := secrets.values[b.CredentialRef]; ok {
		t.Fatal("removed token persisted")
	}
	if !bytes.Equal(secret, secrets.values[original.CredentialRef]) {
		t.Fatal("unrelated token changed")
	}
	if _, err := r.binding(original.ID); err != nil {
		t.Fatal("unrelated binding changed", err)
	}
	if err := r.RemoveUnused(context.Background(), b.ID, nil); err != nil {
		t.Fatal("remove replay", err)
	}
	if err := r.RemoveUnused(context.Background(), original.ID, nil); err != nil {
		t.Fatal(err)
	}
	var doc registryDocument
	if decode(store.data, &doc) != nil || doc.Bindings == nil || len(doc.Bindings) != 0 {
		t.Fatal("empty registry invalid")
	}
}

func TestInvalidSetupStateDoesNotOpenBrowser(t *testing.T) {
	f := newFixture(t)
	r, store, _, _ := registry(f)
	store.data = []byte(`{"schema_version":99}`)
	if _, err := r.Setup(context.Background(), Selection{Endpoint: f.url + "/mcp"}, f.open); err == nil || f.opens.Load() != 0 {
		t.Fatal("opened browser before validating registry")
	}
	if _, err := r.Setup(context.Background(), Selection{Endpoint: "http://remote:3773/mcp"}, f.open); err == nil {
		t.Fatal("remote setup")
	}
	if _, err := r.Setup(context.Background(), Selection{Endpoint: f.url + "/mcp", WSLDistribution: "--bad"}, f.open); err == nil {
		t.Fatal("distribution option injected")
	}
	if err := r.RemoveUnused(context.Background(), "../other", nil); err == nil {
		t.Fatal("invalid binding ID")
	}
	if err := (*Registry)(nil).RemoveUnused(context.Background(), "local", nil); err == nil {
		t.Fatal("nil registry accepted")
	}
}

func TestRedirectAndCrossOriginMetadataNeverReceiveCredential(t *testing.T) {
	var foreign atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreign.Add(1)
		t.Error("request escaped selected origin")
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/mcp", http.StatusFound) }))
	defer server.Close()
	client, err := HTTPClient(context.Background(), server.URL+"/mcp", "test-only-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if response, err := client.Get(server.URL + "/mcp"); err == nil {
		response.Body.Close()
		t.Fatal("redirect followed")
	}
	if foreign.Load() != 0 {
		t.Fatal("foreign origin contacted")
	}
}

func TestEndpointAndRegistryValidation(t *testing.T) {
	for _, raw := range []string{"http://localhost:3773/mcp", "https://127.0.0.1:443/mcp", "http://[::1]:3773/mcp"} {
		if _, err := Endpoint(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://example.com:3773/mcp", "http://192.168.1.2:3773/mcp", "http://localhost/mcp", "http://localhost:0/mcp", "http://localhost:65536/mcp", "http://user@localhost:3773/mcp", "http://localhost:3773/mcp?q=secret", "http://localhost:3773/mcp#secret", "http://localhost:3773/other", "ftp://localhost:3773/mcp", "http://[fe80::1%25eth0]:3773/mcp"} {
		if _, err := Endpoint(raw); err == nil {
			t.Fatal(raw)
		}
	}
	for _, name := range []string{"", "-bad", "bad\nname", strings.Repeat("x", 256), string([]byte{255})} {
		if Distribution(name) {
			t.Fatal("accepted distribution", name)
		}
	}
	if !Distribution("Ubuntu-24.04") || Identifier("../secret") || Identifier("") || Identifier(strings.Repeat("x", 129)) {
		t.Fatal("identifier validation")
	}
	f := newFixture(t)
	r, store, _, b := registry(f)
	for _, data := range [][]byte{[]byte(`null`), []byte(`{"schema_version":1,"bindings":null}`), []byte(`{"schema_version":1,"bindings":[],"extra":1}`), []byte(`{"schema_version":1,"bindings":[]} {}`), bytes.Repeat([]byte(" "), MaxBytes+1)} {
		store.data = data
		if _, err := r.binding("local"); err == nil {
			t.Fatal("invalid registry accepted")
		}
	}
	store.data, _ = json.Marshal(registryDocument{SchemaVersion: 1, Bindings: []Binding{b, b}})
	if _, err := r.binding("local"); err == nil {
		t.Fatal("duplicate binding")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestLocalHTTPBoundaryAndLimits(t *testing.T) {
	var calls atomic.Int32
	carrier := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Header.Get("Authorization") != "Bearer scoped-token" {
			t.Fatal("missing token")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})
	client, err := HTTPClient(context.Background(), "http://localhost:3773/mcp", "scoped-token", carrier)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://example.com:3773/mcp", "http://localhost:9999/mcp", "https://localhost:3773/mcp"} {
		if _, err := client.Get(raw); err == nil {
			t.Fatal("cross-origin request")
		}
	}
	response, err := client.Get("http://localhost:3773/mcp")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	for _, status := range []int{401, 403} {
		client, _ = HTTPClient(context.Background(), "http://localhost:3773/mcp", "scoped-token", roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("sensitive"))}, nil
		}))
		if _, err := client.Get("http://localhost:3773/mcp"); !errors.Is(err, ErrAuthorization) {
			t.Fatal(err)
		}
	}
	for _, known := range []bool{false, true} {
		client, _ = HTTPClient(context.Background(), "http://localhost:3773/mcp", "", roundTripFunc(func(*http.Request) (*http.Response, error) {
			length := int64(-1)
			if known {
				length = MaxBytes + 1
			}
			return &http.Response{StatusCode: 200, ContentLength: length, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", MaxBytes+1)))}, nil
		}))
		response, err := client.Get("http://localhost:3773/mcp")
		if err == nil {
			_, err = io.ReadAll(response.Body)
			response.Body.Close()
		}
		if !errors.Is(err, ErrIncomplete) {
			t.Fatal(err)
		}
	}
	if _, err = HTTPClient(context.Background(), "http://localhost:3773/mcp", "bad\r\ntoken", nil); err == nil {
		t.Fatal("header injection")
	}
}

// Native integration drivers run this bounded, test-only fixture in the already
// running WSL environment. It is not part of a shipped Linux product.
func TestNativeWSLFixtureServer(t *testing.T) {
	if os.Getenv("SNOOZAP_WSL_FIXTURE") != "1" {
		t.Skip("native WSL fixture driver only")
	}
	f := newFixture(t)
	f.identity = map[string]any{"environmentId": "local-environment", "platform": map[string]any{"os": "linux"}}
	f.page = func(map[string]json.RawMessage) any { return page([]any{row("child", "waiting", false)}, nil, 1) }
	if ready := os.Getenv("SNOOZAP_WSL_FIXTURE_READY"); ready != "" {
		if err := os.WriteFile(ready, []byte(f.url), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			return
		case <-t.Context().Done():
			return
		case <-tick.C:
			if stop := os.Getenv("SNOOZAP_WSL_FIXTURE_STOP"); stop != "" {
				if _, err := os.Stat(stop); err == nil {
					return
				}
			}
		}
	}
}
