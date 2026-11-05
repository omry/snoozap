package httpprobe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

func TestTypedPointerConditions(t *testing.T) {
	for _, test := range []struct {
		body, pointer, operator, value string
		state                          model.State
	}{
		{`true`, "", "", "", model.Active},
		{`false`, "", "", "", model.Inactive},
		{`{"a/b":{"~key":[null,true,"busy",3]}}`, "/a~1b/~0key/1", "equals", "true", model.Active},
		{`{"a/b":{"~key":[null,true,"busy",3]}}`, "/a~1b/~0key/0", "equals", "null", model.Active},
		{`{"":false}`, "/", "not_equals", "true", model.Active},
		{`{"s":"busy"}`, "/s", "equals", `"busy"`, model.Active},
		{`{"s":"busy"}`, "/s", "not_equals", `"busy"`, model.Inactive},
		{`{"s":"3"}`, "/s", "not_equals", "3", model.Unknown},
		{`{"s":null}`, "/s", "equals", "false", model.Unknown},
		{`{"s":true}`, "/s/x", "exists", "true", model.Unknown},
		{`{"s":[]}`, "/s", "equals", "true", model.Unknown},
		{`{"s":{}}`, "/s", "exists", "true", model.Active},
		{`{}`, "/missing", "exists", "true", model.Inactive},
		{`{}`, "/missing", "equals", "true", model.Unknown},
		{`[1]`, "/1", "exists", "true", model.Inactive},
		{`[1]`, "/-", "exists", "true", model.Inactive},
		{`[1]`, "/-", "equals", "true", model.Unknown},
		{`[1]`, "/184467440737095516160", "exists", "true", model.Inactive},
		{`[1]`, "/01", "exists", "true", model.Unknown},
		{`[1]`, "/+0", "exists", "true", model.Unknown},
		{`1`, "", "greater_than", "0.99999999999999999999999999", model.Active},
		{`9007199254740993`, "", "equals", "9007199254740992", model.Inactive},
		{`1.2000`, "", "equals", "12e-1", model.Active},
		{`-0e99999999999999999999999999`, "", "equals", "0", model.Active},
		{`-2`, "", "less_than", "-1", model.Active},
		{`2`, "", "greater_or_equal", "2", model.Active},
		{`2`, "", "less_or_equal", "1", model.Inactive},
		{`1e99999999999999999999999999`, "", "greater_than", "1e99999999999999999999999998", model.Active},
		{`1e-99999999999999999999999999`, "", "less_than", "1e-99999999999999999999999998", model.Active},
	} {
		t.Run(fmt.Sprintf("%s%s %s %s", test.body, test.pointer, test.operator, test.value), func(t *testing.T) {
			v, err := decode(context.Background(), []byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			p := config.Probe{Type: "http", URL: "http://localhost", Pointer: test.pointer, Operator: test.operator}
			if test.value != "" {
				p.Value = json.RawMessage(test.value)
			}
			if err := config.ValidateHTTP(p); err != nil {
				t.Fatal(err)
			}
			state, code := evaluate(context.Background(), v, p)
			if state != test.state || state == model.Unknown && code == "" {
				t.Fatalf("%s/%s, want %s", state, code, test.state)
			}
		})
	}
}

func TestDecimalComparisonMatchesRationalArithmetic(t *testing.T) {
	values := []string{"0", "-0", "1", "-1", "1.2", "12e-1", "0.001", "1000", "-1000.01", "9007199254740993", "1e+100", "-1e-100", "1.00000000000000000001"}
	for _, a := range values {
		for _, b := range values {
			x, _ := new(big.Rat).SetString(a)
			y, _ := new(big.Rat).SetString(b)
			if got := compareNumbers(a, b); got != x.Cmp(y) {
				t.Fatalf("%s vs %s: %d", a, b, got)
			}
		}
	}
	// The exponent has thousands of digits, but no exponent-sized integer or
	// rational power is allocated. This must remain a small metadata comparison.
	huge := strings.Repeat("9", 4096)
	if compareNumbers("1e"+huge, "2e"+huge) != -1 || compareNumbers("1e-"+huge, "0") != 1 {
		t.Fatal("huge exponent comparison")
	}
}

func TestJSONExponentBudgetAndDecodeCancellation(t *testing.T) {
	for _, sign := range []string{"", "+", "-"} {
		for _, size := range []int{config.MaxExponentDigits, config.MaxExponentDigits + 1} {
			_, err := decode(context.Background(), []byte("1e"+sign+strings.Repeat("9", size)))
			if (err == nil) != (size == config.MaxExponentDigits) {
				t.Fatalf("exponent sign %q size %d: %v", sign, size, err)
			}
		}
	}
	// A body can fit the byte/depth limits while requiring many decoder steps.
	// Cancellation must interrupt those steps rather than wait for all values.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := decode(ctx, []byte("["+strings.Repeat("0,", 400000)+"0]"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("JSON decoding did not observe cancellation", err)
	}
}

func TestDecodeRejectsIllFormedResponseUnicode(t *testing.T) {
	for _, test := range []struct {
		name, body, pointer, operator, value string
	}{
		{name: "lone high scalar", body: `"\ud800"`, operator: "equals", value: `"�"`},
		{name: "lone low scalar", body: `"\udfff"`, operator: "not_equals", value: `"�"`},
		{name: "lone high string", body: `{"s":"\ud800"}`, pointer: "/s", operator: "equals", value: `"�"`},
		{name: "embedded lone low string", body: `{"s":"pre\udfffsuffix"}`, pointer: "/s", operator: "not_equals", value: `"anything"`},
		{name: "lone high string not equals", body: `{"s":"\ud800"}`, pointer: "/s", operator: "not_equals", value: `"anything"`},
		{name: "lone high object key", body: `{"\ud800":true}`, pointer: "/�", operator: "exists"},
		{name: "lone low object key", body: `{"\udfff":true}`, pointer: "/�", operator: "exists"},
		{name: "high nonlow object key", body: `{"\ud800\u1234":true}`, pointer: "/�ሴ", operator: "exists"},
		{name: "embedded high object key", body: `{"pre\ud800post":true}`, pointer: "/pre�post", operator: "exists"},
		{name: "high followed by nonlow", body: `{"s":"\ud800\u1234"}`, pointer: "/s", operator: "exists"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := config.Probe{Type: "http", URL: "http://localhost", Pointer: test.pointer, Operator: test.operator}
			if test.value != "" {
				probe.Value = json.RawMessage(test.value)
			}
			if err := config.ValidateHTTP(probe); err != nil {
				t.Fatalf("valid comparison configuration rejected: %v", err)
			}
			decoded, err := decode(context.Background(), []byte(test.body))
			if err == nil {
				state, code := evaluate(context.Background(), decoded, probe)
				t.Fatalf("malformed response was normalized to %s/%s", state, code)
			}
		})
	}
}

func TestMalformedResponseCheckReturnsUnknown(t *testing.T) {
	for _, test := range []struct {
		name, body, pointer, operator, value string
	}{
		{name: "string equals", body: `{"s":"\ud800"}`, pointer: "/s", operator: "equals", value: `"�"`},
		{name: "string not equals", body: `{"s":"\udfff"}`, pointer: "/s", operator: "not_equals", value: `"anything"`},
		{name: "object key exists", body: `{"\ud800":true}`, pointer: "/�", operator: "exists"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			server.Listener = listener
			server.Start()
			defer server.Close()
			probe := Probe{Configuration: config.Probe{Type: "http", URL: server.URL, Pointer: test.pointer, Operator: test.operator}}
			if test.value != "" {
				probe.Configuration.Value = json.RawMessage(test.value)
			}
			got := probe.Check(context.Background())
			if got.State != model.Unknown || got.ErrorCode != "invalid_json" {
				t.Fatalf("malformed response was not sanitized: %+v", got)
			}
		})
	}
}

func TestDecodePreservesValidUnicodeControls(t *testing.T) {
	for _, body := range []string{`"\ud83c\udf19"`, `"🌙"`, `"\ufffd"`, `"�"`, `"\\uD800"`} {
		if _, err := decode(context.Background(), []byte(body)); err != nil {
			t.Fatalf("valid JSON string encoding rejected (%s): %v", body, err)
		}
	}
}

func TestComparisonValueExponentBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "1")
	}))
	defer server.Close()
	// The same exponent bound applies to a programmatic comparison value.
	p := Probe{Configuration: config.Probe{URL: server.URL, Value: json.RawMessage("1e" + strings.Repeat("9", config.MaxExponentDigits+1))}}
	if got := p.Check(context.Background()); got.State != model.Unknown || got.ErrorCode != "invalid_configuration" {
		t.Fatal("unbounded comparison value accepted", got)
	}
}

type secrets struct {
	data  []byte
	err   error
	calls int
}

func (s *secrets) Read(context.Context, string) ([]byte, error) { s.calls++; return s.data, s.err }

func TestCredentialedGETAndRedactedResults(t *testing.T) {
	const private = "private-header-value"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != private || r.URL.RawQuery != "key=private-query" {
			t.Error("credentialed GET mismatch")
		}
		io.WriteString(w, `{"working":true}`)
	}))
	defer server.Close()
	for _, test := range []struct {
		secret string
		err    error
		state  model.State
	}{
		{private, nil, model.Active}, {"", nil, model.Unknown}, {strings.Repeat("x", 16385), nil, model.Unknown}, {"bad\r\nHost: elsewhere", nil, model.Unknown}, {"bad\tvalue", nil, model.Unknown}, {private, errors.New("private-storage-error"), model.Unknown},
	} {
		s := &secrets{data: []byte(test.secret), err: test.err}
		p := Probe{Configuration: config.Probe{Type: "http", URL: server.URL + "?key=private-query", Pointer: "/working", CredentialHeaders: map[string]string{"Authorization": "local-key"}}, Credentials: s}
		got := p.Check(context.Background())
		if got.State != test.state || s.calls != 1 {
			t.Fatal(got, s.calls)
		}
		for _, b := range s.data {
			if b != 0 {
				t.Fatal("owned credential bytes retained")
			}
		}
		text := fmt.Sprint(got)
		for _, sensitive := range []string{private, "private-query", "private-storage-error", server.URL} {
			if strings.Contains(text, sensitive) {
				t.Fatal("private diagnostics", got)
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid credential sent", calls.Load())
	}
	p := Probe{Configuration: config.Probe{URL: server.URL, CredentialHeaders: map[string]string{"Authorization": "missing"}}}
	if p.Check(context.Background()).State != model.Unknown {
		t.Fatal("missing credential store accepted")
	}
}

func TestBoundedResponsesAndNoRedirectsOrProxy(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); io.WriteString(w, "true") }))
	defer destination.Close()
	// Transport must ignore proxy environment variables even for a DNS name
	// which is not one of net/http's built-in localhost proxy exceptions.
	t.Setenv("HTTP_PROXY", destination.URL)
	t.Setenv("HTTPS_PROXY", destination.URL)
	for _, test := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"success", "true", 200, ""}, {"boundary", "true" + strings.Repeat(" ", maxBytes-4), 200, ""},
		{"oversized", "true" + strings.Repeat(" ", maxBytes-3), 200, "response_too_large"},
		{"truncated", `{"private-body":`, 200, "invalid_json"}, {"trailing", `true false`, 200, "invalid_json"},
		{"invalid UTF8", "\"bad\xff\"", 200, "invalid_json"},
		{"duplicate", `{"work":true,"work":false}`, 200, "invalid_json"},
		{"depth", strings.Repeat("[", 130) + "true" + strings.Repeat("]", 130), 200, "invalid_json"},
		{"empty", "", 204, "invalid_json"}, {"failure", "private-body", 401, "http_status"}, {"redirect", "", 302, "http_status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			p := Probe{Configuration: config.Probe{URL: "http://fixture.invalid:" + port}, lookup: func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}}
			got := p.Check(context.Background())
			if got.ErrorCode != test.code || got.StatusCode != test.status || (got.State == model.Active) != (test.code == "") {
				t.Fatal(got)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("followed redirect or used proxy")
	}
}

func TestSystemResolverIPv6AndPrematureBodyClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "true") }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if got := (Probe{Configuration: config.Probe{URL: "http://localhost:" + port}}).Check(context.Background()); got.State != model.Active {
		t.Fatal("system localhost resolution", got)
	}
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal("IPv6 loopback fixture", err)
	}
	v6 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "true") }))
	v6.Listener.Close()
	v6.Listener = listener
	v6.Start()
	defer v6.Close()
	if got := (Probe{Configuration: config.Probe{URL: v6.URL}}).Check(context.Background()); got.State != model.Active {
		t.Fatal("IPv6 local GET", got)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\ntrue")
		buffer.Flush()
	}))
	defer broken.Close()
	if got := (Probe{Configuration: config.Probe{URL: broken.URL}}).Check(context.Background()); got.State != model.Unknown || got.ErrorCode != "http_unavailable" {
		t.Fatal("truncated body accepted", got)
	}
	headers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", 65<<10))
		io.WriteString(w, "true")
	}))
	defer headers.Close()
	if got := (Probe{Configuration: config.Probe{URL: headers.URL}}).Check(context.Background()); got.State != model.Unknown || got.ErrorCode != "http_unavailable" {
		t.Fatal("unbounded response headers accepted", got)
	}
	invalid := Probe{Configuration: config.Probe{URL: "http://192.168.56.1/"}, Credentials: &secrets{data: []byte("private")}}
	if invalid.Check(context.Background()).ErrorCode != "invalid_configuration" || invalid.Credentials.(*secrets).calls != 0 {
		t.Fatal("invalid URL accessed credential store")
	}
}

func TestDNSValidationPinsOnlyLoopbackAndRechecksEachPoll(t *testing.T) {
	for _, test := range []struct {
		name string
		ips  []net.IPAddr
		err  error
		dial bool
	}{
		{"v4", []net.IPAddr{{IP: net.ParseIP("127.0.0.2")}}, nil, true},
		{"v6", []net.IPAddr{{IP: net.ParseIP("::1")}}, nil, true},
		{"mapped", []net.IPAddr{{IP: net.ParseIP("::ffff:127.0.0.1")}}, nil, true},
		{"mixed", []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("192.168.56.1")}}, nil, false},
		{"foreign", []net.IPAddr{{IP: net.ParseIP("192.168.56.1")}}, nil, false},
		{"empty", nil, nil, false}, {"failure", nil, errors.New("private-dns-error"), false},
		{"zone", []net.IPAddr{{IP: net.ParseIP("::1"), Zone: "1"}}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			p := Probe{lookup: func(context.Context, string) ([]net.IPAddr, error) { return test.ips, test.err }, dial: func(_ context.Context, network, address string) (net.Conn, error) {
				calls++
				host, port, err := net.SplitHostPort(address)
				if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port != "1234" {
					t.Error("unpinned destination", address)
				}
				return nil, io.ErrClosedPipe
			}}
			_, err := p.dialLoopback(context.Background(), "tcp", "fixture.invalid:1234")
			if err == nil || (calls != 0) != test.dial {
				t.Fatal(err, calls)
			}
		})
	}
	var connections, lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { connections.Add(1); io.WriteString(w, "true") }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	p := Probe{Configuration: config.Probe{URL: "http://fixture.invalid:" + port}, lookup: func(context.Context, string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("192.168.56.1")}}, nil
	}}
	if p.Check(context.Background()).State != model.Active || p.Check(context.Background()).State != model.Unknown || connections.Load() != 1 || lookups.Load() != 2 {
		t.Fatal("reused connection or DNS answer")
	}
}

func TestCancellationSlowBodyAndInvalidTLS(t *testing.T) {
	for _, flush := range []bool{false, true} {
		t.Run(fmt.Sprint(flush), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if flush {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			p := Probe{Configuration: config.Probe{URL: server.URL, TimeoutMS: 30}}
			start := time.Now()
			if p.Check(context.Background()).State != model.Unknown || time.Since(start) > time.Second {
				t.Fatal("deadline not respected")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if p.Check(ctx).State != model.Unknown {
				t.Fatal("cancelled request succeeded")
			}
		})
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "true") }))
	defer tls.Close()
	if got := (Probe{Configuration: config.Probe{URL: tls.URL}}).Check(context.Background()); got.State != model.Unknown {
		t.Fatal("invalid TLS accepted", got)
	}
	roots := x509.NewCertPool()
	roots.AddCert(tls.Certificate())
	if got := (Probe{Configuration: config.Probe{URL: tls.URL}, roots: roots}).Check(context.Background()); got.State != model.Active {
		t.Fatal("trusted TLS failed", got)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(tls.URL, "https://"))
	p := Probe{Configuration: config.Probe{URL: "https://wrong-name.invalid:" + port}, roots: roots, lookup: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}}
	if p.Check(context.Background()).State != model.Unknown {
		t.Fatal("TLS hostname verification bypassed")
	}
}
