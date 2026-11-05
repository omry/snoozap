package t3

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxBytes = 1 << 20

var (
	ErrUnavailable       = errors.New("T3 connection unavailable")
	ErrIdentity          = errors.New("T3 local environment could not be verified")
	ErrAuthorization     = errors.New("T3 authorization unavailable; reconnect locally")
	ErrSchema            = errors.New("unsupported T3 response")
	ErrIncomplete        = errors.New("incomplete T3 observation")
	ErrCredentialCleanup = errors.New("T3 credential cleanup unavailable")
	ErrBindingInUse      = errors.New("T3 environment is still referenced by a probe")
)

// Binding is machine-local metadata. Configuration exports contain only ID.
type Binding struct {
	ID              string `json:"id"`
	Endpoint        string `json:"endpoint"`
	EnvironmentID   string `json:"environment_id"`
	WSLDistribution string `json:"wsl_distribution,omitempty"`
	CredentialRef   string `json:"credential_ref"`
}

func Identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func Distribution(s string) bool {
	return len(s) > 0 && len(s) <= 255 && utf8.ValidString(s) && !strings.HasPrefix(s, "-") && !strings.ContainsFunc(s, unicode.IsControl)
}

// Endpoint accepts one explicit local MCP resource, without a proxy or alias.
func Endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/mcp" || u.RawPath != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrIdentity
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, ErrIdentity
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, ErrIdentity
	}
	return u, nil
}

func validBinding(b Binding) bool {
	_, err := Endpoint(b.Endpoint)
	return err == nil && Identifier(b.ID) && Identifier(b.CredentialRef) && len(b.EnvironmentID) > 0 && len(b.EnvironmentID) <= 256 && utf8.ValidString(b.EnvironmentID) && !strings.ContainsFunc(b.EnvironmentID, unicode.IsControl) && (b.WSLDistribution == "" || Distribution(b.WSLDistribution))
}

type localTransport struct {
	ctx    context.Context
	origin *url.URL
	base   http.RoundTripper
	token  string
}

type limitedBody struct {
	io.ReadCloser
	remaining int
	stop      func() bool
	cancel    context.CancelFunc
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if len(p) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	if n > b.remaining {
		return 0, ErrIncomplete
	}
	b.remaining -= n
	return n, err
}
func (b *limitedBody) Close() error { b.stop(); b.cancel(); return b.ReadCloser.Close() }

func (t *localTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || req.URL.Host != t.origin.Host || req.URL.User != nil || req.URL.Fragment != "" {
		return nil, ErrIdentity
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	r := req.Clone(ctx)
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		stop()
		cancel()
		return nil, ErrUnavailable
	}
	if response.ContentLength > MaxBytes {
		response.Body.Close()
		stop()
		cancel()
		return nil, ErrIncomplete
	}
	if t.token != "" && (response.StatusCode == 401 || response.StatusCode == 403) {
		response.Body.Close()
		stop()
		cancel()
		return nil, ErrAuthorization
	}
	response.Body = &limitedBody{ReadCloser: response.Body, remaining: MaxBytes, stop: stop, cancel: cancel}
	return response, nil
}

func (t *localTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// HTTPClient pins all OAuth and MCP requests to the selected local origin.
// A supplied carrier changes the network namespace, not MCP or OAuth behavior.
func HTTPClient(ctx context.Context, endpoint, token string, carrier http.RoundTripper) (*http.Client, error) {
	u, err := Endpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(token, "\r\n") || len(token) > MaxBytes {
		return nil, ErrAuthorization
	}
	if carrier == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		tr.MaxResponseHeaderBytes = 16 << 10
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		tr.DialContext = func(c context.Context, network, address string) (net.Conn, error) {
			host, port, e := net.SplitHostPort(address)
			if e != nil || host != u.Hostname() || port != u.Port() {
				return nil, ErrIdentity
			}
			if host == "localhost" {
				host = "127.0.0.1"
			}
			return dialer.DialContext(c, network, net.JoinHostPort(host, port))
		}
		carrier = tr
	}
	return &http.Client{Transport: &localTransport{ctx: ctx, origin: u, base: carrier, token: token}, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrIdentity }}, nil
}

func errorCode(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if ctx.Err() != nil {
		return "cancelled"
	}
	switch {
	case errors.Is(err, ErrIdentity):
		return "identity"
	case errors.Is(err, ErrAuthorization):
		return "authorization"
	case errors.Is(err, ErrSchema):
		return "schema"
	case errors.Is(err, ErrIncomplete):
		return "incomplete"
	}
	return "unavailable"
}
