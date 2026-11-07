package t3

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omry/snoozap/internal/model"
)

type Credentials interface {
	Read(context.Context, string) ([]byte, error)
	Write(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

type Registry struct {
	Read    func() ([]byte, error)
	Write   func([]byte) error
	Secrets Credentials
	Verify  func(context.Context, Binding, string) error
	Owner   func(context.Context, Binding) error
	Carrier http.RoundTripper
	mu      sync.Mutex
}

type registryDocument struct {
	SchemaVersion int       `json:"schema_version"`
	Bindings      []Binding `json:"bindings"`
}
type credential struct {
	AccessToken string    `json:"access_token"`
	Expiry      time.Time `json:"expiry"`
	Scope       string    `json:"scope"`
}

func decode(data []byte, v any) error {
	if len(data) > MaxBytes {
		return ErrSchema
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return ErrSchema
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrSchema
	}
	return nil
}

func (r *Registry) load() ([]Binding, error) {
	if r == nil || r.Read == nil {
		return nil, ErrUnavailable
	}
	data, err := r.Read()
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(data) == 0 {
		return []Binding{}, nil
	}
	var doc registryDocument
	if decode(data, &doc) != nil || doc.SchemaVersion != 1 || doc.Bindings == nil || len(doc.Bindings) > 256 {
		return nil, ErrSchema
	}
	ids, refs := map[string]bool{}, map[string]bool{}
	for _, b := range doc.Bindings {
		if !validBinding(b) || ids[b.ID] || refs[b.CredentialRef] {
			return nil, ErrSchema
		}
		ids[b.ID] = true
		refs[b.CredentialRef] = true
	}
	return doc.Bindings, nil
}

func (r *Registry) binding(id string) (Binding, error) {
	if r == nil || !Identifier(id) {
		return Binding{}, ErrIdentity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bindings, err := r.load()
	if err != nil {
		return Binding{}, err
	}
	for _, b := range bindings {
		if b.ID == id {
			return b, nil
		}
	}
	return Binding{}, ErrIdentity
}

// RemoveUnused is an owner operation after the config edit has removed the
// last reference. The owner supplies its current references while serializing
// configuration changes. A failed deletion retains the binding for retry.
func (r *Registry) RemoveUnused(ctx context.Context, id string, references []string) error {
	if r == nil || r.Secrets == nil || r.Write == nil || !Identifier(id) {
		return ErrIdentity
	}
	for _, reference := range references {
		if reference == id {
			return ErrBindingInUse
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bindings, err := r.load()
	if err != nil {
		return err
	}
	for i, b := range bindings {
		if b.ID == id {
			if r.Secrets.Delete(ctx, b.CredentialRef) != nil {
				return ErrCredentialCleanup
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			remaining := append(bindings[:i:i], bindings[i+1:]...)
			if remaining == nil {
				remaining = []Binding{}
			}
			data, err := json.Marshal(registryDocument{SchemaVersion: 1, Bindings: remaining})
			if err != nil {
				return ErrUnavailable
			}
			if r.Write(data) != nil {
				return ErrUnavailable
			}
			return nil
		}
	}
	return nil
}

func resultObject(result *mcp.CallToolResult) (map[string]json.RawMessage, error) {
	if result == nil || result.IsError {
		return nil, ErrSchema
	}
	var data []byte
	if result.StructuredContent != nil {
		var err error
		data, err = json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, ErrSchema
		}
	} else {
		if len(result.Content) != 1 {
			return nil, ErrSchema
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			return nil, ErrSchema
		}
		data = []byte(text.Text)
	}
	if len(data) > MaxBytes {
		return nil, ErrIncomplete
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, ErrSchema
	}
	return object, nil
}

// Identity reads the server's supported API, not a T3 database or window title.
func Identity(ctx context.Context, session *mcp.ClientSession) (string, string, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "t3_environment_read", Arguments: map[string]any{}})
	if err != nil {
		return "", "", err
	}
	object, err := resultObject(result)
	if err != nil {
		return "", "", err
	}
	var id string
	var platform struct {
		OS string `json:"os"`
	}
	if json.Unmarshal(object["environmentId"], &id) != nil || id == "" || len(id) > 256 || json.Unmarshal(object["platform"], &platform) != nil || platform.OS == "" {
		return "", "", ErrSchema
	}
	return id, platform.OS, nil
}

func Connect(ctx context.Context, endpoint string, client *http.Client) (*mcp.ClientSession, error) {
	return mcp.NewClient(&mcp.Implementation{Name: "SnooZap", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: MaxBytes}, nil)
}

func VerifyIdentity(ctx context.Context, b Binding, token string, carrier http.RoundTripper) error {
	client, err := HTTPClient(ctx, b.Endpoint, token, carrier)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	session, err := Connect(ctx, b.Endpoint, client)
	if err != nil {
		return ErrIdentity
	}
	defer session.Close()
	id, platform, err := Identity(ctx, session)
	expected := "windows"
	if b.WSLDistribution != "" {
		expected = "linux"
	}
	if err != nil || id != b.EnvironmentID || platform != expected {
		return ErrIdentity
	}
	return nil
}

type Probe struct {
	Registry    *Registry
	Environment string
}

func (p Probe) Check(ctx context.Context) model.Observation {
	observation := model.Observation{State: model.Unknown}
	err := p.check(ctx, &observation)
	if err != nil {
		observation.State = model.Unknown
		observation.ErrorCode = errorCode(ctx, err)
	}
	return observation
}
func (p Probe) check(ctx context.Context, out *model.Observation) error {
	b, err := p.Registry.binding(p.Environment)
	if err != nil {
		return err
	}
	r := p.Registry
	if r.Secrets == nil || r.Owner == nil {
		return ErrUnavailable
	}
	if err = r.Owner(ctx, b); err != nil {
		return ErrIdentity
	}
	data, err := r.Secrets.Read(ctx, b.CredentialRef)
	if err != nil {
		return ErrAuthorization
	}
	defer clear(data)
	var secret credential
	if decode(data, &secret) != nil || secret.AccessToken == "" || secret.Scope != "orchestration:read" || !secret.Expiry.After(time.Now()) {
		return ErrAuthorization
	}
	client, err := HTTPClient(ctx, b.Endpoint, secret.AccessToken, r.Carrier)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	session, err := Connect(ctx, b.Endpoint, client)
	if err != nil {
		return err
	}
	defer session.Close()
	id, platform, err := Identity(ctx, session)
	expected := "windows"
	if b.WSLDistribution != "" {
		expected = "linux"
	}
	if err != nil {
		return err
	}
	if id != b.EnvironmentID || platform != expected {
		return ErrIdentity
	}
	return observe(ctx, session, b.EnvironmentID, out)
}

func observe(ctx context.Context, session *mcp.ClientSession, environment string, out *model.Observation) error {
	var cursor *int64
	total := -1
	seen := map[string]bool{}
	cursors := map[int64]bool{}
	busy := map[string]bool{"preparing": true, "queued": true, "starting": true, "running": true, "waiting": true}
	known := map[string]bool{"idle": true, "completed": true, "interrupted": true, "failed": true, "cancelled": true, "rolled_back": true}
	for page := 0; page < 32; page++ {
		args := map[string]any{"allProjects": true, "includeSubagents": true, "settled": false, "statuses": []string{"preparing", "queued", "starting", "running", "waiting"}, "limit": 100}
		// The credential's server is the selected environment. Never route a
		// request to a saved remote environment through another tool argument.
		if cursor != nil {
			args["cursor"] = *cursor
		}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "t3_thread_list", Arguments: args})
		if err != nil {
			return err
		}
		object, err := resultObject(result)
		if err != nil {
			return err
		}
		var rows []json.RawMessage
		var next *int64
		var count *int
		if json.Unmarshal(object["threads"], &rows) != nil || rows == nil || len(rows) > 100 || len(object["nextCursor"]) == 0 || json.Unmarshal(object["nextCursor"], &next) != nil || json.Unmarshal(object["total"], &count) != nil || count == nil || *count < 0 {
			return ErrSchema
		}
		if total == -1 {
			total = *count
		}
		if *count != total {
			return ErrIncomplete
		}
		active := false
		for _, raw := range rows {
			var row struct {
				ID          string `json:"threadId"`
				Status      string `json:"status"`
				Settled     *bool  `json:"settled"`
				Environment string `json:"environmentId"`
			}
			if json.Unmarshal(raw, &row) != nil || row.ID == "" || len(row.ID) > 256 || row.Settled == nil || (!busy[row.Status] && !known[row.Status]) {
				return ErrSchema
			}
			if seen[row.ID] {
				return ErrIncomplete
			}
			seen[row.ID] = true
			if (row.Environment == "" || row.Environment == environment) && !*row.Settled && busy[row.Status] {
				active = true
			}
		}
		if len(seen) > total {
			return ErrIncomplete
		}
		if active {
			out.State = model.Active
			return nil
		}
		if next == nil {
			if len(seen) != total {
				return ErrIncomplete
			}
			out.State = model.Inactive
			return nil
		}
		if *next < 0 || cursors[*next] || (cursor != nil && *next <= *cursor) {
			return ErrIncomplete
		}
		cursors[*next] = true
		cursor = next
	}
	return ErrIncomplete
}
