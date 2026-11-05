package t3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

type Selection struct{ Endpoint, WSLDistribution string }
type authorizationResponse struct {
	result *auth.AuthorizationResult
	err    error
}

func randomID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", ErrUnavailable
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

// Setup is called explicitly by the local owner. Polling never calls it.
// It returns an opaque ID only after verified identity and durable secret/binding
// writes. The frontend creates or rebinds its rule through the existing service.
func (r *Registry) Setup(parent context.Context, selection Selection, open func(context.Context, string) error) (binding Binding, err error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	u, err := Endpoint(selection.Endpoint)
	if err != nil || r == nil || r.Owner == nil || r.Verify == nil || r.Secrets == nil || r.Write == nil || open == nil || (selection.WSLDistribution != "" && !Distribution(selection.WSLDistribution)) {
		return Binding{}, ErrIdentity
	}
	r.mu.Lock()
	existing, readErr := r.load()
	r.mu.Unlock()
	if readErr != nil {
		return Binding{}, readErr
	}
	if len(existing) >= 256 {
		return Binding{}, ErrUnavailable
	}
	binding = Binding{Endpoint: selection.Endpoint, WSLDistribution: selection.WSLDistribution}
	if r.Owner(ctx, binding) != nil {
		return Binding{}, ErrIdentity
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return Binding{}, ErrUnavailable
	}
	defer listener.Close()
	callbackID, err := randomID("")
	if err != nil {
		return Binding{}, err
	}
	callbackPath := "/callback/" + callbackID
	redirect := "http://localhost:" + strings.Split(listener.Addr().String(), ":")[1] + callbackPath
	responses := make(chan authorizationResponse, 1)
	var mu sync.Mutex
	expectedState := ""
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			http.Error(w, "Unsupported request", 405)
			return
		}
		q := req.URL.Query()
		mu.Lock()
		state := expectedState
		mu.Unlock()
		if state == "" || len(q["state"]) != 1 || q.Get("state") != state {
			http.Error(w, "Invalid callback", 400)
			return
		}
		response := authorizationResponse{err: ErrAuthorization}
		if q.Get("error") == "" && len(q["code"]) == 1 && q.Get("code") != "" && len(q.Get("code")) <= 4096 {
			response = authorizationResponse{result: &auth.AuthorizationResult{Code: q.Get("code"), State: state, Iss: q.Get("iss")}}
		}
		select {
		case responses <- response:
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("SnooZap received the sign-in response. You can close this window."))
		default:
			http.Error(w, "Response already received", 409)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	client, err := HTTPClient(ctx, binding.Endpoint, "", r.Carrier)
	if err != nil {
		return Binding{}, err
	}
	defer client.CloseIdleConnections()
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL: redirect, Client: client, ScopeFilter: func([]string) []string { return []string{"orchestration:read"} },
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "SnooZap", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none", Scope: "orchestration:read"}},
		AuthorizationCodeFetcher: func(c context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			a, e := url.Parse(args.URL)
			if e != nil || a.Scheme != u.Scheme || a.Host != u.Host || a.User != nil || a.Fragment != "" || a.Query().Get("scope") != "orchestration:read" || a.Query().Get("redirect_uri") != redirect || a.Query().Get("code_challenge_method") != "S256" || a.Query().Get("code_challenge") == "" || a.Query().Get("state") == "" {
				return nil, ErrIdentity
			}
			mu.Lock()
			expectedState = a.Query().Get("state")
			mu.Unlock()
			if open(c, args.URL) != nil {
				return nil, ErrUnavailable
			}
			select {
			case response := <-responses:
				return response.result, response.err
			case <-c.Done():
				return nil, c.Err()
			}
		},
	})
	if err != nil {
		return Binding{}, ErrUnavailable
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "SnooZap", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: binding.Endpoint, HTTPClient: client, OAuthHandler: handler, DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: MaxBytes}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return Binding{}, ctx.Err()
		}
		return Binding{}, ErrAuthorization
	}
	defer session.Close()
	source, err := handler.TokenSource(ctx)
	if err != nil || source == nil {
		return Binding{}, ErrAuthorization
	}
	token, err := source.Token()
	if err != nil {
		return Binding{}, ErrAuthorization
	}
	scope, _ := token.Extra("scope").(string)
	if scope != "orchestration:read" || token.AccessToken == "" || !token.Expiry.After(time.Now()) {
		return Binding{}, ErrAuthorization
	}
	id, platform, err := Identity(ctx, session)
	expectedPlatform := "windows"
	if binding.WSLDistribution != "" {
		expectedPlatform = "linux"
	}
	if err != nil || platform != expectedPlatform {
		return Binding{}, ErrIdentity
	}
	binding.EnvironmentID = id
	if r.Verify(ctx, binding, token.AccessToken) != nil {
		return Binding{}, ErrIdentity
	}
	if binding.ID, err = randomID("t3_"); err != nil {
		return Binding{}, err
	}
	if binding.CredentialRef, err = randomID("t3_token_"); err != nil {
		return Binding{}, err
	}
	if !validBinding(binding) {
		return Binding{}, ErrIdentity
	}
	data, err := json.Marshal(credential{AccessToken: token.AccessToken, Expiry: token.Expiry, Scope: scope})
	if err != nil {
		return Binding{}, ErrAuthorization
	}
	defer clear(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	bindings, err := r.load()
	if err != nil {
		return Binding{}, err
	}
	if len(bindings) >= 256 {
		return Binding{}, ErrUnavailable
	}
	for _, b := range bindings {
		if b.ID == binding.ID || b.CredentialRef == binding.CredentialRef {
			return Binding{}, ErrUnavailable
		}
	}
	temporaryRef := binding.CredentialRef
	defer func() {
		if temporaryRef != "" {
			cleanup, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if r.Secrets.Delete(cleanup, temporaryRef) != nil {
				err = ErrCredentialCleanup
			}
		}
	}()
	if r.Secrets.Write(ctx, binding.CredentialRef, data) != nil {
		return Binding{}, ErrAuthorization
	}
	encoded, err := json.Marshal(registryDocument{SchemaVersion: 1, Bindings: append(bindings, binding)})
	if err != nil {
		return Binding{}, ErrUnavailable
	}
	if ctx.Err() != nil {
		return Binding{}, ctx.Err()
	}
	if r.Write(encoded) != nil {
		return Binding{}, ErrUnavailable
	}
	temporaryRef = ""
	return binding, nil
}
