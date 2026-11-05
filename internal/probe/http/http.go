// Package httpprobe evaluates bounded JSON conditions on loopback HTTP servers.
package httpprobe

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/model"
)

const (
	maxBytes = 1 << 20
)

type Credentials interface {
	Read(context.Context, string) ([]byte, error)
}

type Probe struct {
	Configuration config.Probe
	Credentials   Credentials
	// Test seams stay package-private; production always uses the system
	// resolver and dialer and the standard TLS certificate verification.
	lookup func(context.Context, string) ([]net.IPAddr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	roots  *x509.CertPool
}

func unknown(code string) model.Observation {
	return model.Observation{State: model.Unknown, ErrorCode: code}
}

func (p Probe) Check(ctx context.Context) model.Observation {
	c := p.Configuration
	if config.ValidateHTTP(c) != nil {
		return unknown("invalid_configuration")
	}
	timeout := 2 * time.Second
	if c.TimeoutMS > 0 && c.TimeoutMS <= int64(time.Duration(1<<63-1)/time.Millisecond) {
		timeout = time.Duration(c.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return unknown("invalid_configuration")
	}
	defer clear(request.Header)
	for name, ref := range c.CredentialHeaders {
		if p.Credentials == nil {
			return unknown("credential_unavailable")
		}
		secret, err := p.Credentials.Read(ctx, ref)
		valid := err == nil && len(secret) > 0 && len(secret) <= 16384
		for _, b := range secret {
			if b < 32 || b > 126 {
				valid = false
				break
			}
		}
		if !valid {
			clear(secret)
			return unknown("credential_unavailable")
		}
		request.Header.Set(name, string(secret))
		clear(secret)
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            p.dialLoopback,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig:        &tls.Config{RootCAs: p.roots},
	}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return unknown("http_unavailable")
	}
	defer response.Body.Close()
	result := unknown("http_status")
	result.StatusCode = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		result.ErrorCode = "http_unavailable"
		return result
	}
	if len(data) > maxBytes {
		result.ErrorCode = "response_too_large"
		return result
	}
	if ctx.Err() != nil {
		result.ErrorCode = "http_unavailable"
		return result
	}
	value, err := decode(ctx, data)
	if err != nil {
		result.ErrorCode = "invalid_json"
		return result
	}
	result.State, result.ErrorCode = evaluate(ctx, value, c)
	if ctx.Err() != nil {
		result.State, result.ErrorCode = model.Unknown, "http_unavailable"
	}
	return result
}

// Resolve every new connection, validate the whole answer before any dial, then
// dial only its numeric addresses. DNS cannot change the address after the check.
func (p Probe) dialLoopback(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid local destination")
	}
	var addresses []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		addresses = []net.IPAddr{{IP: ip}}
	} else {
		lookup := p.lookup
		if lookup == nil {
			lookup = net.DefaultResolver.LookupIPAddr
		}
		addresses, err = lookup(ctx, host)
	}
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("local resolution unavailable")
	}
	for _, address := range addresses {
		if !address.IP.IsLoopback() || address.Zone != "" {
			return nil, errors.New("nonlocal destination rejected")
		}
	}
	dial := p.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	for _, address := range addresses {
		connection, err := dial(ctx, network, net.JoinHostPort(address.IP.String(), port))
		if err == nil {
			return connection, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New("local connection unavailable")
}

// Decode one complete, unambiguous JSON value, bounded in both bytes and depth.
func decode(ctx context.Context, data []byte) (any, error) {
	if err := config.ValidateJSONEncoding(data); err != nil {
		return nil, errors.New("invalid JSON encoding")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if depth > 128 {
			return nil, errors.New("JSON depth exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch token {
		case json.Delim('{'):
			object := map[string]any{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, errors.New("invalid object key")
				}
				if _, exists := object[name]; exists {
					return nil, errors.New("duplicate object key")
				}
				value, err := read(depth + 1)
				if err != nil {
					return nil, err
				}
				object[name] = value
			}
			if end, err := d.Token(); err != nil || end != json.Delim('}') {
				return nil, errors.New("invalid object")
			}
			return object, nil
		case json.Delim('['):
			array := []any{}
			for d.More() {
				value, err := read(depth + 1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			if end, err := d.Token(); err != nil || end != json.Delim(']') {
				return nil, errors.New("invalid array")
			}
			return array, nil
		default:
			if _, delim := token.(json.Delim); delim {
				return nil, errors.New("invalid JSON value")
			}
			// Keep arbitrary-precision exponent conversion small enough to
			// finish within the scheduler's cancellation grace. The exponent
			// value can still be enormous; only its encoded length is bounded.
			if number, ok := token.(json.Number); ok {
				if at := strings.IndexAny(string(number), "eE"); at >= 0 {
					exponent := strings.TrimLeft(string(number)[at+1:], "+-")
					if len(exponent) > config.MaxExponentDigits {
						return nil, errors.New("JSON exponent limit exceeded")
					}
				}
			}
			return token, nil
		}
	}
	value, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return value, nil
}

func evaluate(ctx context.Context, value any, c config.Probe) (model.State, string) {
	tokens, err := config.PointerTokens(c.Pointer)
	if err != nil {
		return model.Unknown, "invalid_configuration"
	}
	op := c.Operator
	if op == "" {
		op = "equals"
	}
	for _, token := range tokens {
		var found bool
		switch container := value.(type) {
		case map[string]any:
			value, found = container[token]
		case []any:
			if token == "-" {
				if op == "exists" {
					return model.Inactive, ""
				}
				return model.Unknown, "missing_value"
			}
			if token == "" || token != "0" && token[0] == '0' {
				return model.Unknown, "invalid_pointer"
			}
			for _, b := range token {
				if b < '0' || b > '9' {
					return model.Unknown, "invalid_pointer"
				}
			}
			index, err := strconv.ParseUint(token, 10, 64)
			if err == nil && index < uint64(len(container)) {
				value, found = container[index], true
			}
		default:
			return model.Unknown, "type_mismatch"
		}
		if !found {
			if op == "exists" {
				return model.Inactive, ""
			}
			return model.Unknown, "missing_value"
		}
	}
	if op == "exists" {
		return model.Active, ""
	}
	raw := c.Value
	if raw == nil {
		raw = json.RawMessage("true")
	}
	expected, err := decode(ctx, raw)
	if err != nil {
		return model.Unknown, "invalid_configuration"
	}
	comparison := 0
	switch actual := value.(type) {
	case nil:
		if expected != nil {
			return model.Unknown, "type_mismatch"
		}
	case bool:
		v, ok := expected.(bool)
		if !ok {
			return model.Unknown, "type_mismatch"
		}
		if actual != v {
			comparison = 1
		}
	case string:
		v, ok := expected.(string)
		if !ok {
			return model.Unknown, "type_mismatch"
		}
		comparison = strings.Compare(actual, v)
	case json.Number:
		v, ok := expected.(json.Number)
		if !ok {
			return model.Unknown, "type_mismatch"
		}
		comparison = compareNumbers(string(actual), string(v))
	default:
		return model.Unknown, "type_mismatch"
	}
	matched := false
	switch op {
	case "equals":
		matched = comparison == 0
	case "not_equals":
		matched = comparison != 0
	case "greater_than", "greater_or_equal", "less_than", "less_or_equal":
		if _, ok := value.(json.Number); !ok {
			return model.Unknown, "type_mismatch"
		}
		switch op {
		case "greater_than":
			matched = comparison > 0
		case "greater_or_equal":
			matched = comparison >= 0
		case "less_than":
			matched = comparison < 0
		case "less_or_equal":
			matched = comparison <= 0
		}
	default:
		return model.Unknown, "invalid_configuration"
	}
	if matched {
		return model.Active, ""
	}
	return model.Inactive, ""
}

// Compare exact JSON decimals without float rounding or allocating powers of
// ten proportional to an untrusted exponent. The exponent is only metadata.
func compareNumbers(a, b string) int {
	type decimal struct {
		negative bool
		digits   string
		order    *big.Int
	}
	parse := func(s string) decimal {
		n := decimal{negative: strings.HasPrefix(s, "-"), order: new(big.Int)}
		s = strings.TrimPrefix(s, "-")
		if at := strings.IndexAny(s, "eE"); at >= 0 {
			n.order.SetString(s[at+1:], 10)
			s = s[:at]
		}
		if at := strings.IndexByte(s, '.'); at >= 0 {
			n.order.Sub(n.order, big.NewInt(int64(len(s)-at-1)))
			s = s[:at] + s[at+1:]
		}
		n.digits = strings.TrimLeft(s, "0")
		n.order.Add(n.order, big.NewInt(int64(len(n.digits))))
		if n.digits == "" {
			n.negative = false
		}
		return n
	}
	x, y := parse(a), parse(b)
	if x.negative != y.negative {
		if x.negative {
			return -1
		}
		return 1
	}
	result := 0
	if x.digits == "" || y.digits == "" {
		if x.digits != y.digits {
			if x.digits == "" {
				result = -1
			} else {
				result = 1
			}
		}
	} else if result = x.order.Cmp(y.order); result == 0 {
		for i := 0; i < max(len(x.digits), len(y.digits)); i++ {
			left, right := byte('0'), byte('0')
			if i < len(x.digits) {
				left = x.digits[i]
			}
			if i < len(y.digits) {
				right = y.digits[i]
			}
			if left != right {
				if left < right {
					result = -1
				} else {
					result = 1
				}
				break
			}
		}
	}
	if x.negative {
		return -result
	}
	return result
}
