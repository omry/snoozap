package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omry/snoozap/internal/cli"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/history"
	"github.com/omry/snoozap/internal/model"
)

func TestEmptyConfigurationNeverCreatesWork(t *testing.T) {
	for _, input := range []string{`{"schema_version":1}`, `{"schema_version":1,"probes":{},"rules":[]}`} {
		cfg, err := config.Parse(strings.NewReader(input))
		if err != nil || cfg.SchemaVersion != 1 || len(cfg.Probes) != 0 || len(cfg.Rules) != 0 {
			t.Fatalf("empty configuration: %+v, %v", cfg, err)
		}
	}
	cfg := config.Empty()
	if cfg.SchemaVersion != 1 || cfg.Probes == nil || cfg.Rules == nil {
		t.Fatal("empty configuration must carry explicit version and empty collections")
	}
}

func TestHTTPDefaultsTypedValuesAndPortableReferences(t *testing.T) {
	for _, fields := range []string{"", `,"value":null`, `,"operator":"less_than","value":1e999999999999999999999`} {
		cfg, err := config.Parse(strings.NewReader(`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost:1234/status"` + fields + `,"credential_headers":{"Authorization":"local-key"}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		p := cfg.Probes["web"]
		if p.Pointer != "" || p.IntervalMS != 5000 || p.TimeoutMS != 2000 || p.Operator == "" || p.Value == nil {
			t.Fatal("missing HTTP defaults", p)
		}
		if fields == "" && string(p.Value) != "true" || strings.Contains(fields, "null") && string(p.Value) != "null" {
			t.Fatal("lost scalar default or null", p)
		}
		data, err := config.Export(cfg)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := config.Parse(bytes.NewReader(data))
		if err != nil || decoded.Probes["web"].CredentialHeaders["Authorization"] != "local-key" {
			t.Fatal("portable reference failed", err)
		}
		clone := config.Clone(cfg)
		clone.Probes["web"].Value[0] = '0'
		clone.Probes["web"].CredentialHeaders["Authorization"] = "other"
		if cfg.Probes["web"].CredentialHeaders["Authorization"] != "local-key" || !bytes.Equal(cfg.Probes["web"].Value, decoded.Probes["web"].Value) {
			t.Fatal("clone shares HTTP config memory")
		}
	}
}

func TestHTTPRejectsUnsafeOrIncompatibleConfiguration(t *testing.T) {
	for _, fields := range []string{
		`"url":"https://user:password@localhost/"`, `"url":"http://192.168.56.1/"`, `"url":"file:///tmp/status"`,
		`"url":"http://localhost:0/"`, `"url":"http://localhost:65536/"`, `"url":"http://localhost:/"`,
		`"url":"http://localhost/#fragment"`, `"url":"http://[::1%25lo]/"`,
		`"url":"http://localhost/","pointer":"no-slash"`, `"url":"http://localhost/","pointer":"/~2"`,
		`"url":"http://localhost/","operator":"script"`, `"url":"http://localhost/","operator":"less_than","value":"3"`,
		`"url":"http://localhost/","value":[]`, `"url":"http://localhost/","value":{}`, `"url":"http://localhost/","value":true,"value":false`,
		`"url":"http://localhost/","name":"app"`, `"url":"http://localhost/","source":"ac"`,
		`"url":"http://localhost/","local_environment":"local"`, `"url":"http://localhost/","wsl_distribution":"Ubuntu"`,
		`"url":"http://localhost/","headers":{"Authorization":"inline-secret"}`, `"url":"http://localhost/","credential_headers":{"Host":"key"}`,
		`"url":"http://localhost/","credential_headers":{"Authorization":"secret with spaces"}`, `"url":"http://localhost/","credential_headers":{"Bad\nName":"key"}`,
		`"url":"http://localhost/","credential_headers":{"Authorization":"key","authorization":"other"}`,
		`"url":"http://localhost/","credential_headers":null`, `"url":"http://localhost/","pointer":null`, `"url":"http://localhost/","operator":null`,
		`"url":"http://localhost/","URL":"http://elsewhere/"`, `"url":null`,
	} {
		if _, err := config.Parse(strings.NewReader(`{"schema_version":1,"probes":{"web":{"type":"http",` + fields + `}}}`)); err == nil {
			t.Fatal("unsafe HTTP config accepted", fields)
		}
	}
	for _, name := range []string{"Connection", "Forwarded", "X-Forwarded-Host", "Proxy-Authorization", "Content-Length", "Transfer-Encoding", "Trailer", "TE", "Upgrade"} {
		input := fmt.Sprintf(`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost/","credential_headers":{%q:"key"}}}}`, name)
		if _, err := config.Parse(strings.NewReader(input)); err == nil {
			t.Fatal("routing header accepted", name)
		}
	}
	for _, probe := range []string{`{"type":"power","source":"ac","value":null}`, `{"type":"process","name":"app","pointer":"/x"}`, `{"type":"t3","local_environment":"local","credential_headers":{}}`} {
		if _, err := config.Parse(strings.NewReader(`{"schema_version":1,"probes":{"p":` + probe + `}}`)); err == nil {
			t.Fatal("HTTP fields accepted by another adapter", probe)
		}
	}
}

func TestHTTPResourceLimitsAlsoApplyToProgrammaticCandidates(t *testing.T) {
	for _, p := range []config.Probe{
		{URL: "http://localhost", Value: json.RawMessage(strings.Repeat(" ", config.MaxBytes+1))},
		{URL: "http://localhost", Value: json.RawMessage("true false")},
		{URL: "http://localhost", Pointer: "/" + strings.Repeat("x", 32767)},
		{URL: "http://localhost", Pointer: "/~"},
	} {
		if config.ValidateHTTP(p) == nil {
			t.Fatal("programmatic HTTP resource/JSON limit bypassed")
		}
	}
	p := config.Probe{URL: "http://localhost", CredentialHeaders: map[string]string{}}
	for i := 0; i < 33; i++ {
		p.CredentialHeaders[fmt.Sprintf("X-Key-%d", i)] = "local-key"
	}
	if config.ValidateHTTP(p) == nil {
		t.Fatal("credential header limit bypassed")
	}
}

func TestHTTPComparisonExponentBoundary(t *testing.T) {
	for _, sign := range []string{"", "+", "-"} {
		for _, digits := range []int{config.MaxExponentDigits, config.MaxExponentDigits + 1} {
			value := "1e" + sign + strings.Repeat("9", digits)
			input := `{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost","value":` + value + `}}}`
			_, parseErr := config.Parse(strings.NewReader(input))
			validateErr := config.ValidateHTTP(config.Probe{Type: "http", URL: "http://localhost", Value: json.RawMessage(value)})
			wantAccepted := digits == config.MaxExponentDigits
			if (parseErr == nil) != wantAccepted || (validateErr == nil) != wantAccepted {
				t.Fatalf("exponent sign=%q digits=%d: parse=%v validate=%v", sign, digits, parseErr, validateErr)
			}
		}
	}
}

func TestHTTPComparisonUTF8Validation(t *testing.T) {
	validValue := json.RawMessage(`"café 🌙"`)
	validProbe := config.Probe{Type: "http", URL: "http://localhost", Value: validValue}
	if err := config.ValidateHTTP(validProbe); err != nil {
		t.Fatalf("valid Unicode comparison rejected: %v", err)
	}
	validConfig := `{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost","value":"café 🌙"}}}`
	if _, err := config.Parse(strings.NewReader(validConfig)); err != nil {
		t.Fatalf("valid Unicode configuration rejected: %v", err)
	}

	invalidValue := json.RawMessage([]byte{'"', 'b', 'a', 'd', 0xff, '"'})
	invalidProbe := config.Probe{Type: "http", URL: "http://localhost", Value: invalidValue}
	if err := config.ValidateHTTP(invalidProbe); err == nil {
		t.Fatal("malformed UTF-8 comparison accepted")
	}
	invalidConfig := append([]byte(`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost","value":"bad`), 0xff)
	invalidConfig = append(invalidConfig, []byte(`"}}}`)...)
	if _, err := config.Parse(bytes.NewReader(invalidConfig)); err == nil {
		t.Fatal("malformed UTF-8 configuration accepted")
	}
}

func TestHTTPSelectorUTF8Validation(t *testing.T) {
	validURL := "http://localhost/café/�"
	validPointer := "/café/�"
	validValue := json.RawMessage(`"�"`)
	validProbe := config.Probe{
		Type:             "http",
		Enabled:          true,
		OnStartupUnknown: "wait",
		OnError:          "retain",
		GraceSeconds:     60,
		IntervalMS:       5000,
		TimeoutMS:        2000,
		URL:              validURL,
		Pointer:          validPointer,
		Value:            validValue,
	}
	if err := config.ValidateHTTP(validProbe); err != nil {
		t.Fatalf("valid Unicode selectors rejected: %v", err)
	}
	if err := config.Validate(config.Config{SchemaVersion: 1, Probes: map[string]config.Probe{"web": validProbe}, Rules: []config.Rule{}, Settings: config.Empty().Settings}); err != nil {
		t.Fatalf("valid Unicode candidate rejected: %v", err)
	}
	tokens, err := config.PointerTokens(validPointer)
	if err != nil || !reflect.DeepEqual(tokens, []string{"café", "�"}) {
		t.Fatalf("valid Unicode pointer changed: %v %v", tokens, err)
	}
	validConfig := `{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost/café/�","pointer":"/café/�","value":"�"}}}`
	parsed, err := config.Parse(strings.NewReader(validConfig))
	if err != nil {
		t.Fatalf("valid Unicode configuration rejected: %v", err)
	}
	parsedProbe := parsed.Probes["web"]
	if parsedProbe.URL != validURL || parsedProbe.Pointer != validPointer || !bytes.Equal(parsedProbe.Value, validValue) {
		t.Fatalf("valid Unicode selectors changed: %+v", parsedProbe)
	}

	badByte := byte(0xff)
	badURLConfig := append([]byte(`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost/bad`), badByte)
	badURLConfig = append(badURLConfig, []byte(`"}}}`)...)
	if _, err := config.Parse(bytes.NewReader(badURLConfig)); err == nil {
		t.Fatal("malformed UTF-8 URL accepted by Parse")
	}
	badPointerConfig := append([]byte(`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost","pointer":"/bad`), badByte)
	badPointerConfig = append(badPointerConfig, []byte(`"}}}`)...)
	if _, err := config.Parse(bytes.NewReader(badPointerConfig)); err == nil {
		t.Fatal("malformed UTF-8 pointer accepted by Parse")
	}

	badURL := "http://localhost/bad" + string([]byte{badByte})
	badURLProbe := validProbe
	badURLProbe.URL = badURL
	if err := config.ValidateHTTP(badURLProbe); err == nil {
		t.Fatal("malformed UTF-8 URL accepted by ValidateHTTP")
	}
	if err := config.Validate(config.Config{SchemaVersion: 1, Probes: map[string]config.Probe{"web": badURLProbe}, Rules: []config.Rule{}, Settings: config.Empty().Settings}); err == nil {
		t.Fatal("malformed UTF-8 URL accepted by Validate")
	}
	badPointer := "/bad" + string([]byte{badByte})
	badPointerProbe := validProbe
	badPointerProbe.Pointer = badPointer
	if err := config.ValidateHTTP(badPointerProbe); err == nil {
		t.Fatal("malformed UTF-8 pointer accepted by ValidateHTTP")
	}
	if _, err := config.PointerTokens(badPointer); err == nil {
		t.Fatal("malformed UTF-8 pointer accepted by PointerTokens")
	}
	if err := config.Validate(config.Config{SchemaVersion: 1, Probes: map[string]config.Probe{"web": badPointerProbe}, Rules: []config.Rule{}, Settings: config.Empty().Settings}); err == nil {
		t.Fatal("malformed UTF-8 pointer accepted by Validate")
	}
}

func TestHTTPRejectsIllFormedSurrogateEscapes(t *testing.T) {
	for _, test := range []struct {
		name, field, raw string
		wantErr          bool
	}{
		{name: "value lone high", field: "value", raw: `"\ud800"`, wantErr: true},
		{name: "value lone low", field: "value", raw: `"\udfff"`, wantErr: true},
		{name: "value high nonlow", field: "value", raw: `"\ud800\u1234"`, wantErr: true},
		{name: "URL lone high", field: "url", raw: `"http://localhost/\ud800"`, wantErr: true},
		{name: "URL lone low", field: "url", raw: `"http://localhost/\udfff"`, wantErr: true},
		{name: "URL high nonlow", field: "url", raw: `"http://localhost/\ud800\u1234"`, wantErr: true},
		{name: "URL embedded high", field: "url", raw: `"http://localhost/pre\ud800post"`, wantErr: true},
		{name: "pointer lone high", field: "pointer", raw: `"/\ud800"`, wantErr: true},
		{name: "pointer lone low", field: "pointer", raw: `"/\udfff"`, wantErr: true},
		{name: "pointer high nonlow", field: "pointer", raw: `"/\ud800\u1234"`, wantErr: true},
		{name: "pointer embedded low", field: "pointer", raw: `"/pre\udfffsuffix"`, wantErr: true},
		{name: "valid URL pair", field: "url", raw: `"http://localhost/\ud83c\udf19"`},
		{name: "valid pointer pair", field: "pointer", raw: `"/\ud83c\udf19"`},
		{name: "valid value pair", field: "value", raw: `"\ud83c\udf19"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := `"type":"http",`
			if test.field != "url" {
				probe += `"url":"http://localhost",`
			}
			input := `{"schema_version":1,"probes":{"web":{` + probe + `"` + test.field + `":` + test.raw + `}}}`
			if _, err := config.Parse(strings.NewReader(input)); (err != nil) != test.wantErr {
				t.Fatalf("Parse error=%v, wantErr=%t", err, test.wantErr)
			}
		})
	}
	for n, raw := range []json.RawMessage{json.RawMessage(`"\ud800"`), json.RawMessage(`"\udfff"`), json.RawMessage(`"x\ud800y"`), json.RawMessage(`"\ud800\u1234"`)} {
		t.Run(fmt.Sprintf("programmatic comparison %d", n), func(t *testing.T) {
			if err := config.ValidateHTTP(config.Probe{Type: "http", URL: "http://localhost", Value: raw}); err == nil {
				t.Fatalf("ill-formed surrogate comparison accepted: %s", raw)
			}
		})
	}
	for _, input := range []string{
		`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost/\ud83c\udf19","pointer":"/\ud83c\udf19","value":"\ufffd"}}}`,
		`{"schema_version":1,"probes":{"web":{"type":"http","url":"http://localhost","value":"\\uD800"}}}`,
	} {
		if _, err := config.Parse(strings.NewReader(input)); err != nil {
			t.Fatalf("valid JSON string encoding rejected: %v", err)
		}
	}
}

func TestT3ReferencesAndWSLSelectors(t *testing.T) {
	for _, probe := range []string{
		`{"type":"t3","local_environment":"selected-local"}`,
		`{"type":"process","name":"renderer","wsl_distribution":"Ubuntu-24.04"}`,
		`{"type":"process","executable":"/opt/my app/renderer","wsl_distribution":"Ubuntu-24.04"}`,
	} {
		input := `{"schema_version":1,"probes":{"work":` + probe + `},"rules":[{"id":"work","when":{"probe":"work"}}]}`
		cfg, err := config.Parse(strings.NewReader(input))
		if err != nil {
			t.Fatal(input, err)
		}
		exported, err := config.Export(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = config.Parse(bytes.NewReader(exported)); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(exported, []byte("endpoint")) || bytes.Contains(exported, []byte("credential_ref")) || bytes.Contains(exported, []byte("access_token")) {
			t.Fatal("machine-local binding or credentials exported")
		}
	}
	for _, probe := range []string{
		`{"type":"t3"}`, `{"type":"t3","local_environment":"../other"}`,
		`{"type":"t3","local_environment":"local","name":"app"}`,
		`{"type":"t3","local_environment":"local","wsl_distribution":"Ubuntu"}`,
		`{"type":"t3","local_environment":"local","source":"ac"}`,
		`{"type":"t3","local_environment":"local","url":"http://localhost:3773/mcp"}`,
		`{"type":"t3","local_environment":"local","token":"secret"}`,
		`{"type":"process","name":"app","local_environment":"local"}`,
		`{"type":"process","executable":"relative/path","wsl_distribution":"Ubuntu"}`,
		`{"type":"process","executable":"C:\\app.exe","wsl_distribution":"Ubuntu"}`,
		`{"type":"process","name":"app","wsl_distribution":"-bad"}`,
		`{"type":"process","name":"app","wsl_distribution":"bad\nname"}`,
		`{"type":"power","source":"ac","wsl_distribution":"Ubuntu"}`,
		`{"type":"power","source":"ac","local_environment":"local"}`,
	} {
		if _, err := config.Parse(strings.NewReader(`{"schema_version":1,"probes":{"work":` + probe + `}}`)); err == nil {
			t.Fatal("accepted incompatible or secret-bearing fields", probe)
		}
	}
}

func TestPortableExportUsesOnlyValidatedConfiguration(t *testing.T) {
	c := config.Empty()
	data, err := config.Export(c)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := config.Parse(bytes.NewReader(data))
	if err != nil || !roundtrip.Settings.Monitoring || len(roundtrip.Probes) != 0 {
		t.Fatal("export did not roundtrip", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 4 {
		t.Fatal("unexpected portable fields", err)
	}
	for _, k := range []string{"schema_version", "probes", "rules", "settings"} {
		if _, ok := fields[k]; !ok {
			t.Fatal("missing portable field", k)
		}
	}
	c.SchemaVersion = 99
	if data, err := config.Export(c); err == nil || data != nil {
		t.Fatal("invalid config exported")
	}
}

func TestConsoleHistoryAndSafeExports(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "saved config.json")
	historyPath := filepath.Join(dir, "history.json")
	p := &fakePower{}
	var out, stderr bytes.Buffer
	input := "mode awake\nhistory --json\nhistory\nconfig\t export \"" + cfgPath + "\"\nhistory export " + historyPath + "\nhistory clear\nhistory --json\nstatus --json\nstop\n"
	starts := 0
	code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(input), &out, &stderr, func(ctx context.Context, _ string) (model.Service, error) {
		starts++
		return engine.New(ctx, p, nil, "", manualOptions()), nil
	})
	if code != 0 || stderr.Len() != 0 || starts != 1 {
		t.Fatalf("console exit%d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	var h history.Snapshot
	if err := json.Unmarshal(data, &h); err != nil || h.SchemaVersion != 1 || len(h.Records) == 0 {
		t.Fatal("history export", err)
	}
	jsonLines := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		jsonLines++
		if jsonLines == 2 {
			if line != `{"schema_version":1,"records":[]}` {
				t.Fatal("clear did not empty history", line)
			}
		}
	}
	if jsonLines != 3 {
		t.Fatal("missing status/history JSON", out.String())
	}
	_, active, closed := p.state()
	if active || closed != 1 {
		t.Fatal("diagnostic operations leaked owner")
	}
}

func TestConsoleExportsPreserveExistingFilesAndRejectBadCommands(t *testing.T) {
	for _, command := range []string{"config export", "history export"} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.json")
			if err := os.WriteFile(path, []byte("keep-this-file"), 0600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(command+" "+path+"\nstop\n"), &out, &stderr, func(ctx context.Context, _ string) (model.Service, error) {
				return engine.New(ctx, &fakePower{}, nil, "", manualOptions()), nil
			})
			data, err := os.ReadFile(path)
			if code != 1 || err != nil || string(data) != "keep-this-file" || !strings.Contains(stderr.String(), "new writable file") {
				t.Fatal("export overwrote file", code, err)
			}
		})
	}
	for _, command := range []string{"history bad", "history --json unexpected", "history clear extra", "config export"} {
		var out, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(command+"\nstop\n"), &out, &stderr, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, &fakePower{}, nil, "", manualOptions()), nil
		})
		if code != 2 {
			t.Fatal("invalid diagnostic command accepted", command, code)
		}
	}
}

type diagnosticFailure struct{ model.Service }

func (diagnosticFailure) History(context.Context) (history.Snapshot, error) {
	return history.Snapshot{}, errors.New("private-token-error")
}
func (diagnosticFailure) ClearHistory(context.Context) error {
	return errors.New("private-token-error")
}
func (diagnosticFailure) ExportConfig(context.Context) ([]byte, error) {
	return nil, errors.New("private-token-error")
}

func TestDiagnosticErrorsNeverEchoPrivateServiceErrors(t *testing.T) {
	for _, cmd := range []string{"history", "history clear", "history export ignored.json", "config export ignored.json"} {
		var out, stderr bytes.Buffer
		p := &fakePower{}
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(cmd+"\nstop\n"), &out, &stderr, func(ctx context.Context, _ string) (model.Service, error) {
			return diagnosticFailure{engine.New(ctx, p, nil, "", manualOptions())}, nil
		})
		if code != 1 || strings.Contains(out.String()+stderr.String(), "private-token") {
			t.Fatal("unsafe diagnostic failure", cmd, code)
		}
		_, _, closed := p.state()
		if closed != 1 {
			t.Fatal("failed diagnostics leaked service")
		}
	}
}

type diagnosticBrokenWriter struct{}

func (diagnosticBrokenWriter) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("{")) || bytes.HasPrefix(p, []byte("1970-")) {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestDiagnosticOutputFailureClosesHeldRuntime(t *testing.T) {
	for _, cmd := range []string{"history", "history --json"} {
		p := &fakePower{}
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader("mode awake\n"+cmd+"\n"), diagnosticBrokenWriter{}, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, p, fixedClock{time.Unix(123, 0)}, "", manualOptions()), nil
		})
		_, active, closed := p.state()
		if code != 1 || active || closed != 1 {
			t.Fatal("output failure leaked hold", cmd, code, closed)
		}
	}
}

func TestExportsDoNotFollowDestinationSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "preserve.txt")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "export.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("host cannot create symlink fixture:", err)
	}
	for _, verb := range []string{"config export", "history export"} {
		var out, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(verb+" "+link+"\nstop\n"), &out, &stderr, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, &fakePower{}, nil, "", manualOptions()), nil
		})
		data, err := os.ReadFile(target)
		if code != 1 || err != nil || string(data) != "original" {
			t.Fatal("export followed existing link", code, err)
		}
	}
}

func TestRejectUnsupportedConfiguration(t *testing.T) {
	cases := map[string]string{
		"empty": "", "null": "null", "array": "[]", "scalar": "true",
		"missing version": `{}`, "old version": `{"schema_version":0}`,
		"future version": `{"schema_version":2}`, "string version": `{"schema_version":"1"}`,
		"fraction version": `{"schema_version":1.5}`, "null version": `{"schema_version":null}`,
		"unknown field":          `{"schema_version":1,"shell":"anything"}`,
		"uppercase version":      `{"SCHEMA_VERSION":1}`,
		"mixed case version":     `{"Schema_Version":1}`,
		"version alias override": `{"schema_version":999,"SCHEMA_VERSION":1}`,
		"version alias first":    `{"SCHEMA_VERSION":1,"schema_version":1}`,
		"uppercase probes":       `{"schema_version":1,"PROBES":{}}`,
		"mixed case probes":      `{"schema_version":1,"Probes":{}}`,
		"probes alias override":  `{"schema_version":1,"probes":null,"PROBES":{}}`,
		"uppercase rules":        `{"schema_version":1,"RULES":[]}`,
		"mixed case rules":       `{"schema_version":1,"Rules":[]}`,
		"rules alias override":   `{"schema_version":1,"rules":[{}],"RULES":[]}`,
		"duplicate version":      `{"schema_version":1,"schema_version":1}`,
		"escaped duplicate":      `{"schema_version":1,"\u0073chema_version":1}`,
		"nested duplicate":       `{"schema_version":1,"probes":{"p":{"type":"process","type":"http"}}}`,
		"probes array":           `{"schema_version":1,"probes":[]}`,
		"rules object":           `{"schema_version":1,"rules":{}}`,
		"null probes":            `{"schema_version":1,"probes":null}`,
		"null rules":             `{"schema_version":1,"rules":null}`,
		"nonempty probes":        `{"schema_version":1,"probes":{"p":{"type":"process"}}}`,
		"nonempty rules":         `{"schema_version":1,"rules":[{}]}`,
		"extra value":            `{"schema_version":1} {}`,
		"trailing garbage":       `{"schema_version":1} garbage`,
		"truncated":              `{"schema_version":1`, "trailing comma": `{"schema_version":1,}`,
		"wrong end": `{"schema_version":1]`,
		"deep":      `{"schema_version":1,"probes":` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + "}",
		"oversized": strings.Repeat(" ", config.MaxBytes+1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Parse(strings.NewReader(input)); err == nil {
				t.Fatal("unsupported input accepted")
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("private input detail") }

func TestReadFailureIsBoundedAndSafe(t *testing.T) {
	_, err := config.Parse(failingReader{})
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe or missing read error: %v", err)
	}
	input := `{"schema_version":1,"secret-value":"do-not-echo"}`
	_, err = config.Parse(strings.NewReader(input))
	if err == nil || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("configuration values must not appear in diagnostics: %v", err)
	}
}

type fakePower struct {
	mu                   sync.Mutex
	calls                []bool
	active               bool
	closed               int
	acquireErr, clearErr error
	closeErr             error
}

func (p *fakePower) SetRequired(ctx context.Context, required bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.calls = append(p.calls, required)
	if required && p.acquireErr != nil {
		return p.acquireErr
	}
	if !required && p.clearErr != nil {
		return p.clearErr
	}
	p.active = required
	return nil
}

func (p *fakePower) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	p.active = false
	return p.closeErr
}

func (p *fakePower) state() ([]bool, bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.calls...), p.active, p.closed
}

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type alwaysActiveProbe struct{}

func (alwaysActiveProbe) Check(context.Context) model.Observation {
	return model.Observation{State: model.Active}
}

func newEngine(t *testing.T, power *fakePower, configError string) *engine.Engine {
	t.Helper()
	e := engine.New(context.Background(), power, fixedClock{time.Unix(123, 0)}, configError, manualOptions())
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func TestEngineRejectsOverLimitHTTPComparisonBeforeReplacingCandidate(t *testing.T) {
	valid := config.Empty()
	valid.Probes["web"] = config.Probe{
		Type:             "http",
		Enabled:          true,
		OnStartupUnknown: "wait",
		OnError:          "retain",
		GraceSeconds:     60,
		IntervalMS:       100,
		TimeoutMS:        20,
		URL:              "http://localhost",
		Value:            json.RawMessage("true"),
	}
	valid.Rules = []config.Rule{{ID: "web", Enabled: true, When: config.Expression{Probe: "web"}}}
	if err := config.Validate(valid); err != nil {
		t.Fatal(err)
	}

	power := &fakePower{}
	writes := 0
	options := manualOptions()
	options.Configuration = valid
	options.Resolve = func(probe config.Probe) model.Probe {
		return alwaysActiveProbe{}
	}
	options.Save = func(config.Config) error {
		writes++
		return nil
	}
	e := engine.New(context.Background(), power, fixedClock{time.Unix(123, 0)}, "", options)
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	var ready model.Snapshot
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		ready, err = e.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ready.Activity == model.Active && ready.Hold.Requested && ready.Hold.OSAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("valid HTTP rule did not establish active hold: %+v", ready)
		}
		time.Sleep(5 * time.Millisecond)
	}
	before, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake, Duration: time.Hour})
	if err != nil || before.Deadline == nil {
		t.Fatalf("valid HTTP configuration did not establish manual deadline: %+v %v", before, err)
	}
	beforeConfig, err := e.ExportConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		value  json.RawMessage
		mutate func(*config.Probe)
	}{
		{name: "malformed UTF-8", value: json.RawMessage([]byte{'"', 'b', 'a', 'd', 0xff, '"'})},
		{name: "lone surrogate escape", value: json.RawMessage(`"\ud800"`)},
		{name: "over-limit exponent", value: json.RawMessage("1e+" + strings.Repeat("0", config.MaxExponentDigits+1))},
		{name: "malformed URL", mutate: func(probe *config.Probe) { probe.URL = "http://localhost/bad" + string([]byte{0xff}) }},
		{name: "malformed pointer", mutate: func(probe *config.Probe) { probe.Pointer = "/bad" + string([]byte{0xff}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := config.Clone(valid)
			candidateProbe := candidate.Probes["web"]
			if test.value != nil {
				candidateProbe.Value = test.value
			}
			if test.mutate != nil {
				test.mutate(&candidateProbe)
			}
			candidate.Probes["web"] = candidateProbe
			if _, err := e.Execute(context.Background(), model.Command{Operation: "config", Configuration: &candidate}); err == nil {
				t.Fatalf("invalid candidate was accepted: writes=%d", writes)
			}
			afterConfig, err := e.ExportConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			after, err := e.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if writes != 0 || !bytes.Equal(beforeConfig, afterConfig) || !reflect.DeepEqual(before.Probes, after.Probes) || !reflect.DeepEqual(before.Rules, after.Rules) || before.Activity != after.Activity || !reflect.DeepEqual(before.Hold, after.Hold) || !reflect.DeepEqual(before.Deadline, after.Deadline) {
				t.Fatalf("invalid candidate changed active configuration, observation, hold or deadline: writes=%d before=%+v after=%+v config_equal=%t", writes, before, after, bytes.Equal(beforeConfig, afterConfig))
			}
		})
	}
}

func mode(t *testing.T, e *engine.Engine, m model.Mode) model.Snapshot {
	t.Helper()
	s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: m})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestManualControlsWithEmptyConfiguration(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	s, err := e.Snapshot(context.Background())
	if err != nil || s.Mode != model.Auto || s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted {
		t.Fatalf("startup invented protection: %+v %v", s, err)
	}
	if !s.UpdatedAt.Equal(time.Unix(123, 0)) {
		t.Fatal("engine did not use injected clock")
	}
	s = mode(t, e, model.Awake)
	if !s.DesiredHold || !s.NativeRequestActive || !s.NativeRequestAccepted || s.Reason != "Manual Stay awake" {
		t.Fatalf("manual no-setup hold missing: %+v", s)
	}
	sequence := s.Sequence
	for range 50 {
		s, err = e.Snapshot(context.Background())
		if err != nil || s.Sequence != sequence {
			t.Fatal("reading status changed engine state")
		}
	}
	mode(t, e, model.Awake)
	s = mode(t, e, model.AllowSleep)
	if s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted || !strings.Contains(s.Reason, "does not issue Sleep") {
		t.Fatalf("Allow sleep did not release accurately: %+v", s)
	}
	mode(t, e, model.Auto)
	calls, active, _ := p.state()
	if active || len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("unbalanced/repeated native calls: %v active=%t", calls, active)
	}
}

func TestRejectedAcquisitionCannotClaimProtection(t *testing.T) {
	p := &fakePower{acquireErr: errors.New("access denied")}
	e := newEngine(t, p, "")
	s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Awake})
	if err == nil || s.Mode != model.Awake || !s.DesiredHold || s.NativeRequestActive || s.NativeRequestAccepted || s.NativeError == "" {
		t.Fatalf("rejected request misrepresented: %+v %v", s, err)
	}
	read, err := e.Snapshot(context.Background())
	if err != nil || read.NativeError == "" || read.NativeRequestAccepted {
		t.Fatal("status must succeed while exposing a rejected power request")
	}
	mode(t, e, model.Auto)
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, closed := p.state()
	if closed != 1 {
		t.Fatal("power handle not closed exactly once")
	}
}

func TestFailedReleaseRemainsVisibleAndExitClosesHandle(t *testing.T) {
	p := &fakePower{clearErr: errors.New("clear failed")}
	e := newEngine(t, p, "")
	mode(t, e, model.Awake)
	s, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: model.Auto})
	if err == nil || s.DesiredHold || !s.NativeRequestActive || s.NativeRequestAccepted || s.NativeError == "" {
		t.Fatalf("release failure hidden: %+v %v", s, err)
	}
	if err := e.Close(context.Background()); err == nil {
		t.Fatal("shutdown discarded the release error")
	}
	_, active, closed := p.state()
	if active || closed != 1 {
		t.Fatal("clear failure prevented final handle closure")
	}
	if err := e.Close(context.Background()); err == nil {
		t.Fatal("repeated close lost original cleanup error")
	}
	_, _, closed = p.state()
	if closed != 1 {
		t.Fatal("repeated close duplicated resource cleanup")
	}
}

func TestInvalidStartupConfigurationLeavesManualControlsUsable(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "unsupported configuration schema_version; expected 1")
	s, err := e.Snapshot(context.Background())
	if err != nil || s.ConfigurationError == "" || s.DesiredHold {
		t.Fatal("startup failure missing or created automatic work")
	}
	s = mode(t, e, model.Awake)
	if !s.NativeRequestAccepted || s.ConfigurationError == "" {
		t.Fatal("bad configuration disabled explicit manual control or hid the error")
	}
}

func TestInvalidAndCancelledCommandsHaveNoSideEffect(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	for _, c := range []model.Command{{Operation: "mode", Mode: "invalid"}, {Operation: "shell"}} {
		if _, err := e.Execute(context.Background(), c); err == nil {
			t.Fatal("invalid command accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Execute(ctx, model.Command{Operation: "mode", Mode: model.Awake}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled command accepted")
	}
	calls, active, _ := p.state()
	if len(calls) != 0 || active {
		t.Fatal("invalid/cancelled command changed native state")
	}
}

func TestSnapshotsCannotMutateEngine(t *testing.T) {
	e := newEngine(t, &fakePower{}, "")
	s, _ := e.Snapshot(context.Background())
	s.Limitations[0] = "false external claim"
	s2, _ := e.Snapshot(context.Background())
	if s2.Limitations[0] == s.Limitations[0] {
		t.Fatal("snapshot shares mutable engine data")
	}
}

func TestStopResponseDoesNotClaimAnActiveRequest(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	mode(t, e, model.Awake)
	s, err := e.Execute(context.Background(), model.Command{Operation: "stop"})
	if err != nil || s.NativeRequestActive || s.DesiredHold || s.NativeRequestAccepted || s.Mode != model.Auto {
		t.Fatalf("stop returned stale protection state: %+v %v", s, err)
	}
	if _, err := e.Snapshot(context.Background()); !errors.Is(err, model.ErrStopped) {
		t.Fatal("stopped runtime still accepts status commands")
	}
}

func TestConcurrentControlsRemainSerialized(t *testing.T) {
	p := &fakePower{}
	e := newEngine(t, p, "")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 20 {
				m := model.Auto
				if (i+j)%2 == 0 {
					m = model.Awake
				}
				if _, err := e.Execute(context.Background(), model.Command{Operation: "mode", Mode: m}); err != nil {
					t.Error(err)
				}
				if _, err := e.Snapshot(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, active, closed := p.state()
	for i, v := range calls {
		if v != (i%2 == 0) {
			t.Fatalf("duplicate acquisition or release at %d: %v", i, calls)
		}
	}
	if active || closed != 1 {
		t.Fatal("concurrent work leaked its backend")
	}
}

func TestCancellationAndCloseFailure(t *testing.T) {
	p := &fakePower{closeErr: errors.New("close failed")}
	ctx, cancel := context.WithCancel(context.Background())
	e := engine.New(ctx, p, nil, "", manualOptions())
	mode(t, e, model.Awake)
	cancel()
	deadline, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	for {
		_, err := e.Snapshot(deadline)
		if errors.Is(err, model.ErrStopped) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(deadline); err == nil || err.Error() != "close failed" {
		t.Fatalf("lost close error: %v", err)
	}
	_, active, closed := p.state()
	if active || closed != 1 {
		t.Fatal("cancellation leaked a native resource")
	}
}

func TestConsoleUsesOneServiceAndClosesOnEOF(t *testing.T) {
	p := &fakePower{}
	var out, stderr bytes.Buffer
	starts := 0
	start := func(ctx context.Context, dir string) (model.Service, error) {
		starts++
		if dir != "chosen-directory" {
			t.Fatal("data directory not passed to composition")
		}
		return engine.New(ctx, p, nil, "", manualOptions()), nil
	}
	input := "mode awake\nstatus --json\nstatus --json\n"
	code := cli.Run(context.Background(), []string{"run", "--data-dir", "chosen-directory"}, strings.NewReader(input), &out, &stderr, start)
	if code != 0 || starts != 1 || stderr.Len() != 0 {
		t.Fatalf("console: exit=%d starts=%d stderr=%s", code, starts, stderr.String())
	}
	var statuses []model.Snapshot
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s model.Snapshot
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, s)
	}
	if len(statuses) != 2 || !statuses[0].NativeRequestAccepted || statuses[0].Sequence != statuses[1].Sequence {
		t.Fatalf("bad status output: %s", out.String())
	}
	calls, active, closed := p.state()
	if len(calls) != 2 || !calls[0] || calls[1] || active || closed != 1 {
		t.Fatal("EOF did not balance the shared native request")
	}
}

type awakeNotification struct {
	once  sync.Once
	ready chan struct{}
}

func (w *awakeNotification) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("mode=awake desired=true native_request_active=true accepted=true:")) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestConsoleCancellationWithOpenInputReleasesResources(t *testing.T) {
	p := &fakePower{}
	ctx, cancel := context.WithCancel(context.Background())
	in, commands := io.Pipe()
	t.Cleanup(func() {
		cancel()
		_ = commands.Close()
		_ = in.Close()
	})
	out := &awakeNotification{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"run"}, in, out, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, p, nil, "", manualOptions()), nil
		})
	}()
	if _, err := io.WriteString(commands, "mode awake\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("console did not finish acquiring its power request")
	}
	// Keep input open: cancellation must return without EOF or a stop command.
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cancelled console exited with %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled console remained blocked on open input")
	}
	calls, active, closed := p.state()
	if len(calls) != 2 || !calls[0] || calls[1] || active || closed != 1 {
		t.Fatalf("cancellation did not release exactly once: calls=%v active=%t closed=%d", calls, active, closed)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestConsoleFailuresReleaseResources(t *testing.T) {
	cases := []struct {
		name, input string
		power       *fakePower
		out         io.Writer
		want        int
	}{
		{"unknown command", "shell anything\nstop\n", &fakePower{}, io.Discard, 2},
		{"invalid mode", "mode maybe\n", &fakePower{}, io.Discard, 2},
		{"invalid status", "status --raw\n", &fakePower{}, io.Discard, 2},
		{"oversized input", strings.Repeat("x", 70*1024), &fakePower{}, io.Discard, 1},
		{"broken output", "", &fakePower{}, brokenWriter{}, 1},
		{"rejected request", "mode awake\nstop\n", &fakePower{acquireErr: errors.New("rejected")}, io.Discard, 1},
		{"clear failure", "mode awake\nstop\n", &fakePower{clearErr: errors.New("rejected")}, io.Discard, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := func(ctx context.Context, _ string) (model.Service, error) {
				return engine.New(ctx, tc.power, nil, "", manualOptions()), nil
			}
			code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(tc.input), tc.out, io.Discard, start)
			if code != tc.want {
				t.Fatalf("exit=%d want=%d", code, tc.want)
			}
			_, active, closed := tc.power.state()
			if active || closed != 1 {
				t.Fatal("console failure leaked its runtime")
			}
		})
	}
}

func TestInvalidInvocationNeverStartsOwner(t *testing.T) {
	for _, args := range [][]string{nil, {"mode", "awake"}, {"run", "--unknown"}, {"run", "unexpected"}} {
		started := false
		code := cli.Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string) (model.Service, error) {
			started = true
			return nil, errors.New("must not run")
		})
		if started || code != 2 {
			t.Fatalf("invalid invocation started a runtime: %v", args)
		}
	}
}

func TestOfflineConfigurationCheckAndHelpNeverStartRuntime(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(valid, []byte(`{"schema_version":1,"probes":{},"rules":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte(`{"schema_version":123}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"help"}, 0}, {[]string{"--help"}, 0},
		{[]string{"config", "check", valid}, 0},
		{[]string{"config", "check", invalid}, 2},
		{[]string{"config", "check", filepath.Join(dir, "absent")}, 2},
	} {
		starts := 0
		code := cli.Run(context.Background(), tc.args, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string) (model.Service, error) {
			starts++
			return nil, errors.New("must not acquire a native resource")
		})
		if code != tc.want || starts != 0 {
			t.Fatalf("offline invocation %v: code=%d starts=%d", tc.args, code, starts)
		}
	}
}

type failAfterWriter struct{ remaining int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, io.ErrClosedPipe
	}
	w.remaining--
	return len(p), nil
}

func TestOutputFailureWhileHoldingReleasesRequest(t *testing.T) {
	for _, successfulWrites := range []int{1, 2} {
		p := &fakePower{}
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader("mode awake\nstatus --json\n"), &failAfterWriter{successfulWrites}, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, p, nil, "", manualOptions()), nil
		})
		_, active, closed := p.state()
		if code != 1 || active || closed != 1 {
			t.Fatalf("failed output leaked protection: code=%d active=%t closed=%d", code, active, closed)
		}
	}
}

func TestStartupAndInputFailuresAreOperationalErrors(t *testing.T) {
	code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string) (model.Service, error) {
		return nil, errors.New("native resource rejected")
	})
	if code != 1 {
		t.Fatal("native startup failure reported success")
	}
	p := &fakePower{}
	code = cli.Run(context.Background(), []string{"run"}, failingReader{}, io.Discard, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
		return engine.New(ctx, p, nil, "configuration rejected", manualOptions()), nil
	})
	_, active, closed := p.state()
	if code != 1 || active || closed != 1 {
		t.Fatal("input failure did not close its runtime")
	}
}

func manualOptions() engine.Options {
	return engine.Options{ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }}
}

func TestApplicationRuleDefaultsAndStrictFields(t *testing.T) {
	input := `{"schema_version":1,"probes":{"render":{"type":"process","name":"render.exe"},"ac":{"type":"power","source":"ac"}},"rules":[{"id":"work","when":{"all":[{"probe":"render"},{"probe":"ac"}]}}]}`
	cfg, err := config.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Probes["render"]
	if !p.Enabled || p.IntervalMS != 5000 || p.TimeoutMS != 2000 || p.OnError != "retain" || p.OnStartupUnknown != "wait" || p.GraceSeconds != 60 || !cfg.Rules[0].Enabled || !cfg.Settings.Monitoring || cfg.Settings.Battery.FloorPercent != 20 || cfg.Settings.Battery.ResumePercent != 25 {
		t.Fatalf("public defaults changed: %+v", cfg)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(bytes.NewReader(encoded)); err != nil {
		t.Fatal("valid typed configuration does not round trip:", err)
	}
	bad := []string{
		`{"schema_version":1,"settings":null}`,
		`{"schema_version":1,"settings":{"Monitoring":false}}`,
		`{"schema_version":1,"settings":{"monitoring":null}}`,
		`{"schema_version":1,"settings":{"battery":{"floor_percent":0}}}`,
		`{"schema_version":1,"settings":{"battery":{"floor_percent":25,"resume_percent":25}}}`,
		`{"schema_version":1,"settings":{"battery":{"resume_percent":101}}}`,
		`{"schema_version":1,"settings":{"battery":{"AC_ONLY":true}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","name":"app.exe","enabled":null}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","Name":"app.exe"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","name":"app.exe","source":"ac"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","name":"app.exe","executable":"C:\\app.exe"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","name":"folder/app.exe"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","name":"app\u001b.exe"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"process","executable":"relative.exe"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"solar"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","name":"app.exe"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","timeout_ms":5000}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","interval_ms":-1}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","timeout_ms":1.2}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","grace_seconds":9223372036854775807}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","on_error":"ignore"}}}`,
		`{"schema_version":1,"probes":{"p":{"type":"power","source":"ac","on_startup_unknown":"active"}}}`,
		`{"schema_version":1,"rules":[{"id":"r","when":{"probe":"missing"}}]}`,
		`{"schema_version":1,"rules":[{"id":"r","when":{"all":[]}}]}`,
		`{"schema_version":1,"rules":[{"id":"r","when":{"not":null}}]}`,
		`{"schema_version":1,"rules":[{"id":"r","when":{"all":[],"any":[]}}]}`,
		`{"schema_version":1,"rules":[{"id":"r","when":{"probe":""}}]}`,
	}
	for _, input := range bad {
		if _, err := config.Parse(strings.NewReader(input)); err == nil {
			t.Errorf("invalid application configuration accepted: %s", input)
		}
	}
}
func TestRuleResourceLimitsAndReferenceIntegrity(t *testing.T) {
	prefix := `{"schema_version":1,"probes":{"p":{"type":"power","source":"ac"}},"rules":[{"id":"r","when":`
	deep := `{"probe":"p"}`
	for range 32 {
		deep = `{"not":` + deep + `}`
	}
	if _, err := config.Parse(strings.NewReader(prefix + deep + `} ]}`)); err == nil {
		t.Fatal("expression deeper than 32 accepted")
	}
	cfg := config.Empty()
	for n := range 257 {
		cfg.Probes[fmt.Sprintf("p%d", n)] = config.Probe{Type: "power", Enabled: true, Source: "ac", IntervalMS: 5000, TimeoutMS: 2000, OnError: "retain", OnStartupUnknown: "wait", GraceSeconds: 60}
	}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("more than 256 probes accepted")
	}
	cfg = config.Empty()
	p := config.Probe{Type: "power", Enabled: true, Source: "ac", IntervalMS: 5000, TimeoutMS: 2000, OnError: "retain", OnStartupUnknown: "wait", GraceSeconds: 60}
	cfg.Probes["p"] = p
	for n := range 257 {
		cfg.Rules = append(cfg.Rules, config.Rule{ID: fmt.Sprintf("r%d", n), Enabled: true, When: config.Expression{Probe: "p"}})
	}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("more than 256 rules accepted")
	}
	cfg.Rules = []config.Rule{{ID: "same", When: config.Expression{Probe: "p"}}, {ID: "same", When: config.Expression{Probe: "p"}}}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("duplicate rule identifiers accepted")
	}
	group := make([]config.Expression, 4096)
	for n := range group {
		group[n] = config.Expression{Probe: "p"}
	}
	cfg.Rules = []config.Rule{{ID: "r", When: config.Expression{Any: group}}}
	if err := config.Validate(cfg); err == nil {
		t.Fatal("expression node limit ignored")
	}
}
func TestStoreValidatesBeforeReplacing(t *testing.T) {
	writes := 0
	var saved []byte
	store := config.Store{Replace: func(data []byte) error { writes++; saved = append([]byte(nil), data...); return nil }}
	invalid := config.Empty()
	invalid.SchemaVersion = 2
	if err := store.Save(invalid); err == nil || writes != 0 {
		t.Fatal("invalid candidate reached persistent storage")
	}
	if err := store.Save(config.Empty()); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(bytes.NewReader(saved)); err != nil || writes != 1 {
		t.Fatal("saved config is not a valid complete candidate")
	}
	invalidUTF8 := config.Empty()
	invalidUTF8.Probes["web"] = config.Probe{
		Type:             "http",
		Enabled:          true,
		OnStartupUnknown: "wait",
		OnError:          "retain",
		GraceSeconds:     60,
		IntervalMS:       5000,
		TimeoutMS:        2000,
		URL:              "http://localhost",
		Value:            json.RawMessage([]byte{'"', 'b', 'a', 'd', 0xff, '"'}),
	}
	priorWrites, priorSaved := writes, append([]byte(nil), saved...)
	if err := store.Save(invalidUTF8); err == nil || writes != priorWrites || !bytes.Equal(saved, priorSaved) {
		t.Fatalf("malformed UTF-8 candidate reached persistent storage: err=%v writes=%d", err, writes)
	}
	for _, invalidProbe := range []config.Probe{
		{
			Type:             "http",
			Enabled:          true,
			OnStartupUnknown: "wait",
			OnError:          "retain",
			GraceSeconds:     60,
			IntervalMS:       5000,
			TimeoutMS:        2000,
			URL:              "http://localhost/bad" + string([]byte{0xff}),
			Value:            json.RawMessage("true"),
		},
		{
			Type:             "http",
			Enabled:          true,
			OnStartupUnknown: "wait",
			OnError:          "retain",
			GraceSeconds:     60,
			IntervalMS:       5000,
			TimeoutMS:        2000,
			URL:              "http://localhost",
			Pointer:          "/bad" + string([]byte{0xff}),
			Value:            json.RawMessage("true"),
		},
		{
			Type:             "http",
			Enabled:          true,
			OnStartupUnknown: "wait",
			OnError:          "retain",
			GraceSeconds:     60,
			IntervalMS:       5000,
			TimeoutMS:        2000,
			URL:              "http://localhost",
			Value:            json.RawMessage(`"prefix\ud800suffix"`),
		},
	} {
		candidate := config.Empty()
		candidate.Probes["web"] = invalidProbe
		priorWrites, priorSaved := writes, append([]byte(nil), saved...)
		if err := store.Save(candidate); err == nil || writes != priorWrites || !bytes.Equal(saved, priorSaved) {
			t.Fatalf("malformed selector candidate reached persistent storage: err=%v writes=%d", err, writes)
		}
	}
	store.Replace = func([]byte) error { return errors.New("disk failure") }
	if err := store.Save(config.Empty()); err == nil {
		t.Fatal("failed replacement reported success")
	}
}

func TestStoreRefusesSerializedConfigurationOverParserLimit(t *testing.T) {
	candidate := config.Empty()
	for n := range 256 {
		candidate.Probes[fmt.Sprintf("p%d", n)] = config.Probe{
			Type:             "process",
			Enabled:          true,
			OnStartupUnknown: "wait",
			OnError:          "retain",
			GraceSeconds:     60,
			IntervalMS:       5000,
			TimeoutMS:        2000,
			Name:             strings.Repeat("x", 4000),
		}
	}
	if err := config.Validate(candidate); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) <= config.MaxBytes {
		t.Fatalf("test candidate is not oversized: %d", len(encoded))
	}

	writes := 0
	var saved []byte
	store := config.Store{Replace: func(data []byte) error {
		writes++
		saved = append([]byte(nil), data...)
		return nil
	}}
	if err := store.Save(config.Empty()); err != nil {
		t.Fatal(err)
	}
	prior := append([]byte(nil), saved...)
	if err := store.Save(candidate); err == nil || writes != 1 || !bytes.Equal(saved, prior) {
		t.Fatalf("oversized candidate was persisted: err=%v writes=%d saved=%d prior=%d", err, writes, len(saved), len(prior))
	}
	if _, err := config.Parse(bytes.NewReader(saved)); err != nil {
		t.Fatalf("prior persisted configuration was damaged: %v", err)
	}
}
func TestConsoleTimedModesAndConfigApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "application config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"settings":{"monitoring":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	p := &fakePower{}
	var out, stderr bytes.Buffer
	writes := 0
	code := cli.Run(context.Background(), []string{"run"}, strings.NewReader("mode awake --for 1h\nmonitor off\nconfig apply \""+path+"\"\nstatus --json\nstop\n"), &out, &stderr, func(ctx context.Context, _ string) (model.Service, error) {
		options := manualOptions()
		options.Save = func(config.Config) error { writes++; return nil }
		return engine.New(ctx, p, nil, "", options), nil
	})
	if code != 0 || writes != 2 || !strings.Contains(out.String(), `"mode":"awake"`) || !strings.Contains(out.String(), `"monitoring":false`) || !strings.Contains(out.String(), `"deadline":`) || stderr.Len() != 0 {
		t.Fatalf("new console flow failed: code=%d writes=%d stdout=%s stderr=%s", code, writes, out.String(), stderr.String())
	}
	for _, command := range []string{"mode auto --for 1h", "mode awake --for 0s", "mode awake --for -1m", "mode awake --for garbage", "mode awake --wrong 1h"} {
		code := cli.Run(context.Background(), []string{"run"}, strings.NewReader(command+"\nstop\n"), io.Discard, io.Discard, func(ctx context.Context, _ string) (model.Service, error) {
			return engine.New(ctx, &fakePower{}, nil, "", manualOptions()), nil
		})
		if code != 2 {
			t.Fatalf("invalid timer %q returned %d", command, code)
		}
	}
}
