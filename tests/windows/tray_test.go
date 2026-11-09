//go:build windows

package windows_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"github.com/omry/snoozap/frontend"
	"github.com/omry/snoozap/internal/app"
	"github.com/omry/snoozap/internal/desktop"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/ipc"
	"github.com/omry/snoozap/internal/model"
	native "github.com/omry/snoozap/internal/platform/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

func statusCall(method string, args ...any) string {
	body, _ := json.Marshal(map[string]any{"object": 0, "method": 0, "args": map[string]any{
		"call-id": "guard-test", "methodName": "github.com/omry/snoozap/internal/desktop.StatusBridge." + method, "args": args,
	}})
	return string(body)
}

func TestStatusGuardAuthorizesBeforeDispatchAndPrivateResponse(t *testing.T) {
	for _, tc := range []struct {
		name, method, origin, contentType, path, body, chunk, site string
		allowed                                                    bool
	}{
		{name: "read", allowed: true},
		{name: "mode", body: statusCall("Mode", "awake", "1h30m"), allowed: true},
		{name: "no-origin", origin: "-"},
		{name: "opaque-origin", origin: "null"},
		{name: "file-origin", origin: "file://"},
		{name: "external-origin", origin: "https://example.com"},
		{name: "arbitrary-localhost", origin: "http://localhost"},
		{name: "local-ip", origin: "http://127.0.0.1"},
		{name: "other-port", origin: "http://wails.localhost:1234"},
		{name: "prefix-origin", origin: "http://wails.localhost.evil"},
		{name: "https-origin", origin: "https://wails.localhost"},
		{name: "image-get", method: "GET", path: "/wails/runtime?object=0&method=0&args={}"},
		{name: "simple-post", contentType: "text/plain"},
		{name: "form-post", contentType: "application/x-www-form-urlencoded"},
		{name: "options", method: "OPTIONS"},
		{name: "query-dispatch", path: "/wails/runtime?object=0"},
		{name: "chunk", chunk: "external"},
		{name: "cross-site", site: "cross-site"},
		{name: "unknown-method", body: statusCall("Close")},
		{name: "wrong-arity", body: statusCall("Mode", "awake")},
		{name: "other-object", body: `{"object":4,"method":0,"args":{}}`},
		{name: "method-id", body: `{"object":0,"method":0,"args":{"call-id":"x","methodID":1,"args":[]}}`},
		{name: "missing-dispatch", body: `{"args":{}}`},
		{name: "extra-document", body: statusCall("Read") + `{}`},
		{name: "oversized", body: statusCall("Mode", "awake", strings.Repeat("1", 4096))},
		{name: "unknown-route", path: "/wails/events"},
		{name: "source-files", path: "/status.ts"},
		{name: "npm-files", path: "/node_modules/jsdom/package.json"},
		{name: "host-alias", path: "http://wails.localhost.evil/wails/runtime"},
		{name: "host-port", path: "http://wails.localhost:1234/wails/runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, origin, contentType, path, body := tc.method, tc.origin, tc.contentType, tc.path, tc.body
			if method == "" {
				method = "POST"
			}
			if origin == "" {
				origin = "http://wails.localhost"
			}
			if contentType == "" {
				contentType = "application/json"
			}
			if path == "" {
				path = "/wails/runtime"
			}
			if body == "" {
				body = statusCall("Read")
			}
			if strings.HasPrefix(path, "/") {
				path = "http://wails.localhost" + path
			}
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			if origin != "-" {
				req.Header.Set("Origin", origin)
			}
			req.Header.Set("Content-Type", contentType)
			if tc.chunk != "" {
				req.Header.Set("X-Wails-Chunk-Id", tc.chunk)
			}
			req.Header.Set("Sec-Fetch-Site", tc.site)
			reached := false
			handler := desktop.StatusMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if _, err := io.ReadAll(r.Body); err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write([]byte("PRIVATE-FIXTURE-ONLY"))
			}))
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if reached != tc.allowed || strings.Contains(out.Body.String(), "PRIVATE-FIXTURE-ONLY") != tc.allowed {
				t.Fatalf("dispatch/private response allowed=%v reached=%v code=%v body=%s", tc.allowed, reached, out.Code, out.Body.String())
			}
			if !tc.allowed && out.Code < 400 {
				t.Fatal("denial reported success")
			}
			if out.Header().Get("Access-Control-Allow-Origin") != "" || !strings.Contains(out.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing response isolation", out.Header())
			}
		})
	}
}

func TestStatusBundledAssetsHaveNoDevelopmentFallback(t *testing.T) {
	handler := desktop.StatusMiddleware(frontend.Handler())
	for _, path := range []string{"/", "/index.html", "/styles.css", "/status.js"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, httptest.NewRequest(method, "http://wails.localhost"+path, nil))
			if method == "POST" {
				if out.Code != 405 {
					t.Fatal(path, out.Code)
				}
				continue
			}
			if out.Code != 200 || method == "GET" && out.Body.Len() == 0 || method == "HEAD" && out.Body.Len() != 0 {
				t.Fatal(path, method, out.Code, out.Body.Len())
			}
		}
	}
}

type statusCountingService struct {
	model.Service
	count int
	last  model.Command
}

func (s *statusCountingService) Execute(ctx context.Context, cmd model.Command) (model.Snapshot, error) {
	s.count++
	s.last = cmd
	return s.Service.Execute(ctx, cmd)
}

func TestStatusCustomUsesSharedEngineDurationValidationAndOneCommand(t *testing.T) {
	service := &statusCountingService{Service: engine.New(context.Background(), &trayPower{}, failingSuspendClock{origin: time.Now().Add(-time.Second)}, "", engine.Options{ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }})}
	updates, update := trayUpdates()
	controller := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = controller.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	bridge := desktop.NewStatusBridge(controller)
	initial := bridge.Read()
	for _, duration := range []string{"0", "0s", "-1m", "garbage", "1", "1d", "999999999999999999999h", "1h;stop", " 1h", "NaN", "1e9s"} {
		result, err := bridge.Mode(model.Awake, duration)
		if !errors.Is(err, model.ErrInvalidInput) || result.State.Snapshot.Sequence != initial.State.Snapshot.Sequence || service.count != 0 {
			t.Fatal("invalid custom input mutated state", duration, err, service.count)
		}
	}
	for _, mode := range []model.Mode{model.Auto, model.AllowSleep, "stop"} {
		if _, err := bridge.Mode(mode, "1h"); !errors.Is(err, model.ErrInvalidInput) || service.count != 0 {
			t.Fatal("unrelated command dispatched", mode, err)
		}
	}
	result, err := bridge.Mode(model.Awake, "1h30m")
	if err != nil || service.count != 1 || service.last.Duration != 90*time.Minute || result.State.Snapshot.Mode != model.Awake || result.State.Snapshot.Deadline == nil || !result.State.Snapshot.NativeRequestAccepted {
		t.Fatal("custom confirmation diverged from engine", result, err, service.count, service.last)
	}
	if _, err := bridge.Mode(model.Auto, ""); err != nil || service.count != 2 {
		t.Fatal(err, service.count)
	}
	// A valid parsed duration near MaxInt64 still passes through the Service's
	// elapsed-clock overflow bound; the bridge does not bypass it.
	before := bridge.Read()
	_, err = bridge.Mode(model.Awake, "9223372036854775807ns")
	if err == nil || bridge.Read().State.Snapshot.Mode != before.State.Snapshot.Mode {
		t.Fatal("Service duration bound bypassed", err)
	}
}

// Run the real bundled module and stock WebView2 in a separate test process.
// Only this fixture's fake power backend is reachable; no owner lock is taken.
func TestNativeStatusBundledWindowAndOriginIsolation(t *testing.T) {
	requireNative(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestStatusWebviewFixtureChild$", "-test.v", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "SNOOZAP_STATUS_FIXTURE=1", "SNOOZAP_STATUS_FIXTURE_DATA="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("STATUS_NATIVE_ISOLATION_PASSED")) {
		t.Fatalf("isolated bundled WebView fixture: %v\n%s", err, out)
	}
	t.Log(string(out))
}

type statusPrivateService struct {
	model.Service
	commands atomic.Int32
}

func (*statusPrivateService) private(s model.Snapshot) model.Snapshot {
	s.Reason = "PRIVATE-FIXTURE-STATUS <img class=injection src=x>"
	return s
}
func (s *statusPrivateService) Execute(ctx context.Context, command model.Command) (model.Snapshot, error) {
	s.commands.Add(1)
	value, err := s.Service.Execute(ctx, command)
	return s.private(value), err
}
func (s *statusPrivateService) Subscribe(ctx context.Context) (<-chan model.Snapshot, error) {
	source, err := s.Service.Subscribe(ctx)
	if err != nil {
		return nil, err
	}
	result := make(chan model.Snapshot)
	go func() {
		defer close(result)
		for {
			select {
			case <-ctx.Done():
				return
			case value, ok := <-source:
				if !ok {
					return
				}
				select {
				case result <- s.private(value):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return result, nil
}

func TestStatusWebviewFixtureChild(t *testing.T) {
	if os.Getenv("SNOOZAP_STATUS_FIXTURE") != "1" {
		t.Skip("isolated WebView fixture child only")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr := w32.CoInitializeEx(w32.COINIT_APARTMENTTHREADED); hr != 0 && hr != 1 {
		t.Fatalf("fixture COM initialization failed: %x", hr)
	}
	defer w32.CoUninitialize()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &statusPrivateService{Service: engine.New(ctx, &trayPower{}, nil, "", engine.Options{ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }})}
	controller := desktop.NewController(ctx, service, func(desktop.State) {})
	defer controller.Close()
	labels := make(chan string, 32)
	result := make(chan map[string]bool, 1)
	var denied atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/result" {
			var value map[string]bool
			if json.NewDecoder(r.Body).Decode(&value) == nil {
				select {
				case result <- value:
				default:
				}
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<!doctype html><title>Untrusted fixture</title><p>External document</p><script>
const call={object:0,method:0,args:{'call-id':'external-read',methodName:'github.com/omry/snoozap/internal/desktop.StatusBridge.Read',args:[]}};
const mode={object:0,method:0,args:{'call-id':'external-mode',methodName:'github.com/omry/snoozap/internal/desktop.StatusBridge.Mode',args:['awake','4h']}};
let leak=false;let loaded=true;
const body=JSON.stringify(call), target='http://wails.localhost/wails/runtime';
fetch(target,{method:'POST',headers:{'Content-Type':'application/json'},body}).then(r=>r.text()).then(t=>{leak ||= t.includes('PRIVATE-FIXTURE-STATUS')}).catch(()=>{});
fetch(target,{method:'POST',mode:'no-cors',headers:{'Content-Type':'text/plain'},body:JSON.stringify(mode)}).catch(()=>{});
let image=new Image();image.src=target+'?object=0&method=0&args='+encodeURIComponent(JSON.stringify(call.args));document.body.append(image);
let frame=document.createElement('iframe');frame.srcdoc='<script>fetch("'+target+'",{method:"POST",mode:"no-cors",headers:{"Content-Type":"text/plain"},body:'+JSON.stringify(body)+'}).catch(()=>{})<\/script>';document.body.append(frame);
setTimeout(()=>{leak ||= document.body.innerText.includes('PRIVATE-FIXTURE-STATUS');fetch('/result',{method:'POST',body:JSON.stringify({external_loaded:loaded,private_state_leaked:leak})})},1800);
</script>`)
	}))
	defer external.Close()
	url, _ := json.Marshal(external.URL)
	// Appending a test driver keeps the actual compiled interface and CSP intact.
	driver := strings.ReplaceAll(statusNativeDriver, "EXTERNAL_FIXTURE_URL", string(url))
	assets := frontend.Handler()
	a := application.New(application.Options{Name: "SnooZap isolated status acceptance", Services: []application.Service{application.NewService(desktop.NewStatusBridge(controller))},
		Windows: application.WindowsOptions{WndClass: "SnooZapIsolatedStatusFixture", WebviewUserDataPath: os.Getenv("SNOOZAP_STATUS_FIXTURE_DATA")},
		Assets: application.AssetOptions{Middleware: func(next http.Handler) http.Handler {
			guarded := desktop.StatusMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wails/runtime" {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(body))
					var value struct {
						Args struct {
							ID string `json:"call-id"`
						} `json:"args"`
					}
					_ = json.Unmarshal(body, &value)
					if strings.HasPrefix(value.Args.ID, "fixture-") {
						labels <- value.Args.ID
					}
				}
				next.ServeHTTP(w, r)
			}))
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wails/runtime" && r.Header.Get("Origin") != "http://wails.localhost" {
					denied.Add(1)
				}
				guarded.ServeHTTP(w, r)
			})
		}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/status.js" {
				assets.ServeHTTP(w, r)
				return
			}
			out := httptest.NewRecorder()
			assets.ServeHTTP(out, r)
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(out.Body.Bytes())
			_, _ = io.WriteString(w, "\n"+driver)
		})},
	})
	a.Window.NewWithOptions(application.WebviewWindowOptions{Title: "SnooZap isolated status acceptance", URL: "/", Width: 620, Height: 720, DevToolsEnabled: false})
	completed := make(chan error, 1)
	go func() {
		var failure error
		select {
		case value := <-result:
			if !value["external_loaded"] || value["private_state_leaked"] || denied.Load() < 4 || service.commands.Load() != 4 {
				failure = fmt.Errorf("native boundary: result=%v denied=%d Service commands=%d", value, denied.Load(), service.commands.Load())
			}
		case <-time.After(20 * time.Second):
			failure = errors.New("native status fixture timed out")
		}
		observed := []string{}
		for len(labels) > 0 {
			observed = append(observed, <-labels)
		}
		if len(observed) != 1 || observed[0] != "fixture-ui-passed" {
			failure = fmt.Errorf("native UI results=%v; boundary=%v", observed, failure)
		}
		completed <- failure
		application.InvokeAsync(a.Quit)
	}()
	if err := a.Run(); err != nil {
		t.Fatal(err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	t.Log("STATUS_NATIVE_ISOLATION_PASSED: bundled UI, positive custom/replacement/expiry, invalid/cancel/overflow, external JSON/simple POST, image GET and frame; fake power only")
}

const statusNativeDriver = `
(async()=>{
const wait=async(test)=>{for(let i=0;i<100;i++){if(test())return;await new Promise(r=>setTimeout(r,50))}throw Error('UI wait failed')};
const doc=document, field=doc.getElementById('duration'), form=doc.getElementById('custom'), modes=doc.getElementById('modes');
const submit=(value)=>{field.value=value;form.dispatchEvent(new Event('submit',{cancelable:true,bubbles:true}))};
const call=async(name,args,id)=>{const r=await fetch('/wails/runtime',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({object:0,method:0,args:{'call-id':id,methodName:'github.com/omry/snoozap/internal/desktop.StatusBridge.'+name,args}})});if(!r.ok)throw Error('binding '+r.status);return r.json()};
await wait(()=>!modes.disabled);
if(!doc.getElementById('reason').textContent.includes('PRIVATE-FIXTURE-STATUS')||doc.querySelector('.injection'))throw Error('private response/text render');
submit('1h30m');await wait(()=>!modes.disabled&&doc.getElementById('selected').textContent.includes('Stay awake'));
let first=await call('Read',[],'native-observe-1');if(first.State.Snapshot.remaining_ms<5390000)throw Error('wrong duration');
doc.getElementById('cancel').click();if(field.value!==''||doc.activeElement!==field)throw Error('cancel/focus');
for(const value of ['invalid','999999999999999999999h']){submit(value);await wait(()=>!modes.disabled&&doc.getElementById('error').textContent.includes('not confirmed'))}
let unchanged=await call('Read',[],'native-observe-2');if(unchanged.State.Snapshot.deadline!==first.State.Snapshot.deadline)throw Error('invalid/cancel changed deadline');
submit('150ms');await wait(()=>!modes.disabled&&doc.getElementById('selected').textContent==='Selected: Auto');
doc.querySelector('[data-mode="allow-sleep"]').click();await wait(()=>!modes.disabled&&doc.getElementById('selected').textContent.includes('Allow sleep'));
doc.querySelector('[data-mode="auto"]').click();await wait(()=>!modes.disabled&&doc.getElementById('selected').textContent==='Selected: Auto');
await call('Read',[],'fixture-ui-passed');location.href=EXTERNAL_FIXTURE_URL;
})().catch(async(error)=>{await fetch('/wails/runtime',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({object:0,method:0,args:{'call-id':'fixture-ui-failed-'+String(error).slice(0,40),methodName:'github.com/omry/snoozap/internal/desktop.StatusBridge.Read',args:[]}})})});
`

func TestTrayStatusSeparatesSelectedModeFromAcceptedProtection(t *testing.T) {
	accepted := model.Snapshot{SchemaVersion: 1, Sequence: 1, Mode: model.Awake, Activity: model.Inactive, DesiredHold: true, NativeRequestActive: true, NativeRequestAccepted: true, Hold: model.Hold{Requested: true, OSAccepted: true}, Reason: "Manual Stay awake"}
	for _, tc := range []struct {
		name, title, icon string
		change            func(*desktop.State)
	}{
		{"accepted", "Keeping awake", "accepted", func(*desktop.State) {}},
		{"request-rejected", "Sleep prevention failed", "failed", func(s *desktop.State) { s.Snapshot.NativeError = "Windows power request failed" }},
		{"battery-cutoff", "Sleep prevention paused", "paused", func(s *desktop.State) {
			s.Snapshot.Battery = model.Battery{Blocked: true, Reason: "Battery floor protection"}
		}},
		{"desired-only", "Protection unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot.Hold.OSAccepted = false }},
		{"native-not-accepted", "Protection unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot.NativeRequestAccepted = false }},
		{"native-not-active", "Protection unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot.NativeRequestActive = false }},
		{"disconnected", "Owner unavailable", "unknown", func(s *desktop.State) { s.Connected = false }},
		{"allow-sleep", "Normal sleep allowed", "allowed", func(s *desktop.State) { s.Snapshot = model.Snapshot{Mode: model.AllowSleep, Activity: model.Active} }},
		{"inactive", "Normal sleep allowed", "allowed", func(s *desktop.State) { s.Snapshot = model.Snapshot{Mode: model.Auto, Activity: model.Inactive} }},
		{"startup-unknown", "Activity unconfirmed", "unknown", func(s *desktop.State) { s.Snapshot = model.Snapshot{Mode: model.Auto, Activity: model.Unknown} }},
		{"invalid-config", "Activity unconfirmed", "unknown", func(s *desktop.State) {
			s.Snapshot = model.Snapshot{Mode: model.Auto, Activity: model.Inactive, ConfigurationError: "Startup configuration rejected"}
		}},
		{"retained-unknown", "Keeping awake", "accepted", func(s *desktop.State) { s.Snapshot.Mode, s.Snapshot.Activity = model.Auto, model.Unknown }},
		{"startup-hold-unknown", "Keeping awake", "accepted", func(s *desktop.State) {
			s.Snapshot.Mode, s.Snapshot.Activity = model.Auto, model.Unknown
			s.Snapshot.Rules = []model.RuleStatus{{ID: "work", State: model.Unknown, Startup: true}}
		}},
		{"mixed-hold-unknown", "Keeping awake", "accepted", func(s *desktop.State) {
			s.Snapshot.Mode, s.Snapshot.Activity = model.Auto, model.Unknown
			s.Snapshot.Rules = []model.RuleStatus{
				{ID: "startup", State: model.Unknown, Startup: true},
				{ID: "retained", State: model.Unknown, Retained: true},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := desktop.State{Snapshot: accepted, Connected: true}
			tc.change(&s)
			p := desktop.Describe(s)
			if p.Title != tc.title || p.Icon != tc.icon {
				t.Fatalf("presentation: %+v", p)
			}
			if tc.name == "accepted" && (!strings.Contains(p.Request, "Windows accepted") || !strings.Contains(p.Request, "lid")) {
				t.Fatal("accepted API result lost its platform limitation", p)
			}
			if tc.name == "retained-unknown" && !strings.Contains(p.Reason, "unconfirmed") {
				t.Fatal("retained work presented as freshly confirmed", p)
			}
			if tc.name == "startup-hold-unknown" && (!strings.Contains(p.Reason, "startup") || strings.Contains(p.Reason, "last confirmed")) {
				t.Fatal("startup protection was presented as retained work", p)
			}
			if tc.name == "mixed-hold-unknown" && (!strings.Contains(p.Reason, "startup") || !strings.Contains(p.Reason, "last confirmed")) {
				t.Fatal("mixed startup and retained causes were not both presented", p)
			}
		})
	}
	limitations := desktop.Describe(desktop.State{Connected: true, Snapshot: acceptedSnapshotWithLimitations()})
	if len(limitations.Limitations) != len(runtimeLimitations()) {
		t.Fatal("runtime limitations were not retained in presentation", limitations)
	}
	for _, limitation := range limitations.Limitations {
		if len(utf16.Encode([]rune(limitation))) > 120 || strings.IndexFunc(limitation, unicode.IsControl) >= 0 {
			t.Fatal("runtime limitations were unsafe or overlong", limitations)
		}
	}
	deadline := time.Now().Add(time.Minute)
	p := desktop.Describe(desktop.State{Connected: true, Snapshot: model.Snapshot{Mode: model.Awake, Deadline: &deadline, RemainingMS: 60001, Reason: "Manual"}})
	if !strings.Contains(p.Selected, "1m 01s remaining; then Auto") {
		t.Fatal("timer rounding or expiry destination", p)
	}
	p = desktop.Describe(desktop.State{Message: strings.Repeat("\U0001f600\n\t\x00", 100)})
	if len(utf16.Encode([]rune(p.Tooltip))) > 120 || strings.IndexFunc(p.Tooltip+p.Reason, unicode.IsControl) >= 0 {
		t.Fatal("unsafe or overlong native text")
	}
}

func acceptedSnapshotWithLimitations() model.Snapshot {
	return model.Snapshot{
		SchemaVersion:         1,
		Mode:                  model.Awake,
		Activity:              model.Inactive,
		DesiredHold:           true,
		NativeRequestActive:   true,
		NativeRequestAccepted: true,
		Hold:                  model.Hold{Requested: true, OSAccepted: true},
		Reason:                "Manual Stay awake",
		Limitations:           runtimeLimitations(),
	}
}

func runtimeLimitations() []string {
	return []string{
		"An accepted request does not guarantee protection from explicit Sleep or lid close.",
		"Windows Modern Standby and battery policies can limit SystemRequired requests.",
		"SnooZap does not request that the display stay on.",
		"Periodic resume detection unavailable; native notifications remain active.",
	}
}

type failingSuspendClock struct{ origin time.Time }

type failingSuspendTimer struct{ *time.Timer }

func (t failingSuspendTimer) C() <-chan time.Time { return t.Timer.C }

func (c failingSuspendClock) Now() time.Time { return time.Now() }

func (c failingSuspendClock) Elapsed() time.Duration { return time.Since(c.origin) }

func (c failingSuspendClock) NewTimer(d time.Duration) model.Timer {
	return failingSuspendTimer{time.NewTimer(d)}
}

func (failingSuspendClock) SleepTime() (time.Duration, error) {
	return 0, errors.New("fixture suspend clock unavailable")
}

func TestActualEngineLimitationsRemainVisible(t *testing.T) {
	clock := failingSuspendClock{origin: time.Now()}
	service := engine.New(context.Background(), &trayPower{}, clock, "", engine.Options{
		ReadPower: func(context.Context) (model.Battery, error) {
			return model.Battery{Known: true, Source: "ac"}, nil
		},
	})
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assertLimitations := func(label string, snapshot model.Snapshot) {
		if len(snapshot.Limitations) != len(runtimeLimitations()) {
			t.Fatalf("%s engine limitations: %#v", label, snapshot.Limitations)
		}
		presentation := desktop.Describe(desktop.State{Connected: true, Snapshot: snapshot})
		for _, limitation := range runtimeLimitations() {
			found := false
			for _, rendered := range presentation.Limitations {
				if strings.Contains(rendered, limitation) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s presentation omitted %q: %q", label, limitation, presentation.Limitations)
			}
		}
		for _, limitation := range presentation.Limitations {
			if len(utf16.Encode([]rune(limitation))) > 120 || strings.IndexFunc(limitation, unicode.IsControl) >= 0 {
				t.Fatalf("%s presentation is unsafe or overlong: %q", label, limitation)
			}
		}
	}
	auto, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal("actual engine Auto snapshot", err)
	}
	assertLimitations("Auto", auto)
	if auto.NativeRequestAccepted {
		t.Fatalf("Auto snapshot unexpectedly accepted protection: %+v", auto)
	}
	if got := desktop.Describe(desktop.State{Connected: true, Snapshot: auto}).Request; got != "No accepted sleep-prevention request" {
		t.Fatalf("Auto presentation blurred acceptance distinction: %q", got)
	}
	manual, err := service.Execute(ctx, model.Command{Operation: "mode", Mode: model.Awake})
	if err != nil {
		t.Fatal("actual engine manual snapshot", err)
	}
	assertLimitations("manual", manual)
	if !manual.NativeRequestAccepted {
		t.Fatalf("manual snapshot did not retain accepted protection: %+v", manual)
	}
	if got := desktop.Describe(desktop.State{Connected: true, Snapshot: manual}).Request; !strings.Contains(got, "Windows accepted") {
		t.Fatalf("manual presentation lost acceptance distinction: %q", got)
	}
	unsafe := manual
	unsafe.Limitations = []string{"unsafe\ntext\x00 &"}
	presentation := desktop.Describe(desktop.State{Connected: true, Snapshot: unsafe})
	if len(presentation.Limitations) != 1 || strings.IndexFunc(presentation.Limitations[0], unicode.IsControl) >= 0 || !strings.Contains(presentation.Limitations[0], " &") {
		t.Fatalf("limitation sanitization or ampersand preservation failed: %q", presentation.Limitations)
	}
}

type trayPower struct {
	mu       sync.Mutex
	required bool
	closed   int
	fail     atomic.Bool
}

func (p *trayPower) SetRequired(_ context.Context, required bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail.Load() {
		return errors.New("fixture native rejection")
	}
	p.required = required
	return nil
}
func (p *trayPower) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.required = false
	p.closed++
	return nil
}

func trayUpdates() (chan desktop.State, func(desktop.State)) {
	updates := make(chan desktop.State, 32)
	return updates, func(s desktop.State) {
		select {
		case updates <- s:
		default:
		}
	}
}

func waitTray(t *testing.T, updates <-chan desktop.State, predicate func(desktop.State) bool) desktop.State {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case s := <-updates:
			if predicate(s) {
				return s
			}
		case <-deadline.C:
			t.Fatal("tray did not reach the required state")
		}
	}
}

func TestTrayControllerModesTimersErrorsAndOwnedCleanup(t *testing.T) {
	power := &trayPower{}
	service := engine.New(context.Background(), power, nil, "", engine.Options{ReadPower: func(context.Context) (model.Battery, error) {
		return model.Battery{Source: "ac", Known: true}, nil
	}})
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	for _, mode := range []model.Mode{model.Awake, model.AllowSleep, model.Auto} {
		if err := c.Execute(model.Command{Operation: "mode", Mode: mode}); err != nil {
			t.Fatal(err)
		}
		s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.Mode == mode })
		if s.Snapshot.NativeRequestAccepted != (mode == model.Awake) {
			t.Fatal("mode did not reach actual engine power state", s)
		}
	}
	for _, duration := range []time.Duration{time.Hour, 60 * time.Millisecond} {
		if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake, Duration: duration}); err != nil {
			t.Fatal(err)
		}
		waitTray(t, updates, func(s desktop.State) bool {
			return !s.Pending && s.Snapshot.Deadline != nil && s.Snapshot.RemainingMS <= duration.Milliseconds()
		})
	}
	waitTray(t, updates, func(s desktop.State) bool {
		return s.Snapshot.Mode == model.Auto && s.Snapshot.Deadline == nil && !s.Snapshot.Hold.Requested
	})
	if err := c.Execute(model.Command{Operation: "config"}); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatal("tray controller dispatched an unrelated command", err)
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: "invalid"}); err == nil {
		t.Fatal("invalid mode accepted")
	}
	power.fail.Store(true)
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err == nil {
		t.Fatal("fixture OS rejection hidden")
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.NativeError != "" })
	if desktop.Describe(s).Title != "Sleep prevention failed" || s.Snapshot.Hold.OSAccepted {
		t.Fatal("selected mode overclaimed protection", s)
	}
	power.fail.Store(false)
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal("repeated close", err)
	}
	power.mu.Lock()
	defer power.mu.Unlock()
	if power.required || power.closed != 1 {
		t.Fatal("owned power lifetime unbalanced", power.required, power.closed)
	}
}

type trayBlockingService struct {
	model.Service
	started chan struct{}
	count   atomic.Int32
}

func (s *trayBlockingService) Execute(ctx context.Context, _ model.Command) (model.Snapshot, error) {
	s.count.Add(1)
	close(s.started)
	<-ctx.Done()
	return model.Snapshot{}, ctx.Err()
}

func TestTrayControllerDoesNotQueueOrReplayUncertainCommands(t *testing.T) {
	service := &trayBlockingService{Service: engine.New(context.Background(), &trayPower{}, nil, ""), started: make(chan struct{})}
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	result := make(chan error, 1)
	go func() { result <- c.Execute(model.Command{Operation: "mode", Mode: model.Awake}) }()
	select {
	case <-service.started:
	case <-time.After(time.Second):
		t.Fatal("command not dispatched")
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.AllowSleep}); !errors.Is(err, model.ErrUnavailable) {
		t.Fatal("second mutation queued", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) || service.count.Load() != 1 {
		t.Fatal("uncertain command replayed", err, service.count.Load())
	}
}

type trayDelayedService struct {
	model.Service
	updates          chan model.Snapshot
	started, release chan struct{}
	subscribeError   bool
}

func (s *trayDelayedService) Subscribe(context.Context) (<-chan model.Snapshot, error) {
	if s.subscribeError {
		return nil, model.ErrUnavailable
	}
	return s.updates, nil
}

func (s *trayDelayedService) Execute(ctx context.Context, _ model.Command) (model.Snapshot, error) {
	close(s.started)
	select {
	case <-ctx.Done():
		return model.Snapshot{}, ctx.Err()
	case <-s.release:
		return model.Snapshot{SchemaVersion: 1, Sequence: 2, Mode: model.Awake, DesiredHold: true,
			NativeRequestActive: true, NativeRequestAccepted: true, Hold: model.Hold{Requested: true, OSAccepted: true}}, nil
	}
}

func (*trayDelayedService) Close(context.Context) error { return nil }

func TestTrayControllerLateReplyCannotRestoreTerminatedSubscription(t *testing.T) {
	for _, setupFailure := range []bool{false, true} {
		name := "disconnect"
		if setupFailure {
			name = "subscribe-failure"
		}
		t.Run(name, func(t *testing.T) {
			service := &trayDelayedService{updates: make(chan model.Snapshot, 1), started: make(chan struct{}), release: make(chan struct{}), subscribeError: setupFailure}
			service.updates <- model.Snapshot{SchemaVersion: 1, Sequence: 1, Mode: model.Auto}
			updates, update := trayUpdates()
			c := desktop.NewController(context.Background(), service, update)
			t.Cleanup(func() { _ = c.Close() })
			if setupFailure {
				waitTray(t, updates, func(s desktop.State) bool { return !s.Connected && s.Message != "" })
			} else {
				waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
			}
			result := make(chan error, 1)
			go func() { result <- c.Execute(model.Command{Operation: "mode", Mode: model.Awake}) }()
			select {
			case <-service.started:
			case <-time.After(time.Second):
				t.Fatal("command did not start")
			}
			if !setupFailure {
				close(service.updates)
				waitTray(t, updates, func(s desktop.State) bool { return !s.Connected })
			}
			close(service.release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending })
			if s.Connected || desktop.Describe(s).Title != "Owner unavailable" || desktop.Describe(s).Icon == "accepted" {
				t.Fatal("late reply restored terminated observation", s, desktop.Describe(s))
			}
		})
	}
}

func TestNativeTrayCommandFailureKeepsHealthySubscription(t *testing.T) {
	requireNative(t)
	owner := launch(t, executable(t), t.TempDir())
	var failNext atomic.Bool
	client := ipc.NewClient(func(ctx context.Context) (ipc.Conn, error) {
		if failNext.Swap(false) {
			return nil, model.ErrUnavailable
		}
		return ipc.Dial(ctx)
	})
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), client, update)
	t.Cleanup(func() { _ = c.Close() })
	before := waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	failNext.Store(true)
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); !errors.Is(err, model.ErrUnavailable) {
		t.Fatal("fixture command failure was not returned", err)
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Message != "" })
	if !s.Connected || s.Snapshot.Sequence != before.Snapshot.Sequence || s.Snapshot.Mode != before.Snapshot.Mode {
		t.Fatal("command failure lost a healthy subscription's unchanged status", before, s)
	}
	if desktop.Describe(s).Title == "Owner unavailable" || !strings.Contains(s.Message, "not confirmed") {
		t.Fatal("command uncertainty was presented as lost connectivity", s)
	}
	if current := owner.status(t, ""); current.Sequence != before.Snapshot.Sequence || current.Mode != before.Snapshot.Mode {
		t.Fatal("failed command changed the owner state", current)
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal("a fresh command could not use the healthy owner", err)
	}
	waitTray(t, updates, func(s desktop.State) bool {
		return s.Connected && !s.Pending && s.Message == "" && s.Snapshot.Mode == model.Awake
	})
}

func TestNativeTrayControllerAttachesAndDetachPreservesConsoleOwner(t *testing.T) {
	requireNative(t)
	owner := launch(t, executable(t), t.TempDir())
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), ipc.NewClient(ipc.Dial), update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.NativeRequestAccepted })
	fromConsole := owner.status(t, "")
	if fromConsole.Mode != s.Snapshot.Mode || !fromConsole.NativeRequestAccepted {
		t.Fatal("console and attached tray disagree")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !owner.status(t, "").NativeRequestAccepted {
		t.Fatal("closing an attached view stopped the console owner")
	}
	observations, observe := trayUpdates()
	attached := desktop.NewController(context.Background(), ipc.NewClient(ipc.Dial), observe)
	t.Cleanup(func() { _ = attached.Close() })
	waitTray(t, observations, func(s desktop.State) bool { return s.Connected && s.Snapshot.NativeRequestAccepted })
	owner.stop(t)
	s = waitTray(t, observations, func(s desktop.State) bool { return !s.Connected })
	if desktop.Describe(s).Title != "Owner unavailable" {
		t.Fatal("owner disconnect retained the accepted view", s)
	}
}

func TestNativeTrayOwnedExternalStopReleasesOwner(t *testing.T) {
	requireNative(t)
	trayExe := os.Getenv("SNOOZAP_TRAY_EXE")
	consoleExe := os.Getenv("SNOOZAP_CONSOLE_EXE")
	if trayExe == "" || consoleExe == "" {
		t.Skip("SNOOZAP_TRAY_EXE and SNOOZAP_CONSOLE_EXE must name the built Windows fixtures")
	}
	if !filepath.IsAbs(trayExe) || !filepath.IsAbs(consoleExe) {
		t.Fatal("native fixture paths must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	localAppData := t.TempDir()
	fixtureEnv := func() []string {
		env := make([]string, 0, len(os.Environ())+1)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(entry), "LOCALAPPDATA=") {
				env = append(env, entry)
			}
		}
		return append(env, "LOCALAPPDATA="+localAppData)
	}
	cli := func(args ...string) (int, string) {
		cmd := exec.CommandContext(ctx, consoleExe, args...)
		cmd.Env = fixtureEnv()
		output, err := cmd.CombinedOutput()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode(), string(output)
			}
			t.Fatal("console fixture command failed to launch", err)
		}
		return 0, string(output)
	}
	status := func() (int, model.Snapshot, string) {
		code, output := cli("status", "--json")
		var snapshot model.Snapshot
		if code == 0 {
			if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
				t.Fatalf("status output was not JSON: %v (%q)", err, output)
			}
		}
		return code, snapshot, output
	}
	waitStatus := func(wantCode int) model.Snapshot {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			code, snapshot, output := status()
			if code == wantCode {
				return snapshot
			}
			if code != 0 && wantCode != 0 && code != 3 {
				t.Fatalf("unexpected status code=%d output=%q", code, output)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("status did not reach exit code %d", wantCode)
		return model.Snapshot{}
	}
	start := func(label string) (*exec.Cmd, *os.File) {
		log, err := os.Create(filepath.Join(t.TempDir(), label+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, trayExe)
		cmd.Env = fixtureEnv()
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal("tray fixture failed to start", err)
		}
		return cmd, log
	}
	waitExit := func(cmd *exec.Cmd) {
		if err := cmd.Wait(); err != nil {
			log, _ := os.ReadFile(cmd.Stdout.(*os.File).Name())
			t.Fatalf("tray fixture exited with error: %v\n%s", err, log)
		}
	}
	if code, _, output := status(); code != 3 {
		t.Fatalf("owner preflight was not no-instance: code=%d output=%q", code, output)
	}
	tray, trayLog := start("owned-stop")
	trayWaited := false
	t.Cleanup(func() {
		if trayLog != nil {
			trayLog.Close()
		}
		if !trayWaited && tray.ProcessState == nil {
			_, _ = cli("stop")
			_ = tray.Wait()
		}
	})
	initial := waitStatus(0)
	if initial.Mode != model.Auto || initial.NativeRequestActive || initial.ConfigurationError != "" {
		t.Fatalf("empty tray owner was not safe Auto: %+v", initial)
	}
	if code, output := cli("stop"); code != 0 {
		t.Fatalf("external stop failed: code=%d output=%q", code, output)
	}
	waitExit(tray)
	trayWaited = true
	if code, _, output := status(); code != 3 {
		t.Fatalf("status after external stop was not no-instance: code=%d output=%q", code, output)
	}
	fresh, freshLog := start("fresh-owner")
	freshWaited := false
	t.Cleanup(func() {
		if freshLog != nil {
			freshLog.Close()
		}
		if !freshWaited && fresh.ProcessState == nil {
			_, _ = cli("stop")
			_ = fresh.Wait()
		}
	})
	freshStatus := waitStatus(0)
	if freshStatus.Mode != model.Auto {
		t.Fatalf("fresh owner did not start in Auto: %+v", freshStatus)
	}
	if code, output := cli("stop"); code != 0 {
		t.Fatalf("fresh owner external stop failed: code=%d output=%q", code, output)
	}
	waitExit(fresh)
	freshWaited = true
	if code, _, output := status(); code != 3 {
		t.Fatalf("final status was not no-instance: code=%d output=%q", code, output)
	}
}

func TestNativeTraySharedCompositionIsSingleAndReleasesOwner(t *testing.T) {
	requireNative(t)
	service, err := app.Start(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	second, err := app.Start(context.Background(), t.TempDir())
	if second != nil || !errors.Is(err, native.ErrAlreadyRunning) {
		t.Fatal("second composition bypassed the user owner", second, err)
	}
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	waitTray(t, updates, func(s desktop.State) bool { return s.Snapshot.NativeRequestAccepted })
	if err := c.Execute(model.Command{Operation: "stop"}); err != nil {
		t.Fatal("Quit did not stop the actual shared runtime", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := native.AcquireOwner()
	if err != nil {
		t.Fatal("shutdown retained native ownership", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

var trayUser32 = syscall.NewLazyDLL("user32.dll")

func trayCall(name string, args ...uintptr) uintptr {
	r, _, _ := trayUser32.NewProc(name).Call(args...)
	return r
}

// Find only a window created by this exact fixture process. HWND_MESSAGE (-3)
// selects message-only tray windows; native popup menus are top-level windows.
func trayWindow(pid int, parent uintptr, class string) uintptr {
	name, _ := syscall.UTF16PtrFromString(class)
	var previous uintptr
	for {
		hwnd := trayCall("FindWindowExW", parent, previous, uintptr(unsafe.Pointer(name)), 0)
		if hwnd == 0 {
			return 0
		}
		var owner uint32
		trayCall("GetWindowThreadProcessId", hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) == pid {
			return hwnd
		}
		previous = hwnd
	}
}

func trayStatusWindow(pid int) uintptr {
	class, _ := syscall.UTF16PtrFromString("SnooZapTrayWindow")
	title, _ := syscall.UTF16PtrFromString("SnooZap")
	var previous uintptr
	for {
		hwnd := trayCall("FindWindowExW", 0, previous, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
		if hwnd == 0 {
			return 0
		}
		var owner uint32
		trayCall("GetWindowThreadProcessId", hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) == pid {
			return hwnd
		}
		previous = hwnd
	}
}

func statusAccessibility(t *testing.T, hwnd uintptr, action string) {
	t.Helper()
	// UI Automation stays under the already PID-verified fixture window. It
	// neither sends global keystrokes nor changes Windows foreground policy.
	script := fmt.Sprintf(`
$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue'
Add-Type -AssemblyName UIAutomationClient
Add-Type -AssemblyName UIAutomationTypes
$root=[System.Windows.Automation.AutomationElement]::FromHandle([IntPtr]%d)
$condition=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::AutomationIdProperty,'duration')
$input=$null
for($i=0;$i -lt 80;$i++){$input=$root.FindFirst([System.Windows.Automation.TreeScope]::Descendants,$condition);if($null -ne $input){break};Start-Sleep -Milliseconds 50}
if($null -eq $input){throw 'bundled duration field not accessible'}
$action='%s'
if($action -eq 'focus'){
$input.SetFocus();for($i=0;$i -lt 40 -and -not $input.Current.HasKeyboardFocus;$i++){Start-Sleep -Milliseconds 50}
if(-not $input.Current.HasKeyboardFocus){throw 'duration focus failed'}
}
elseif($action -eq 'check-focus'){
for($i=0;$i -lt 40 -and -not $input.Current.HasKeyboardFocus;$i++){Start-Sleep -Milliseconds 50}
if(-not $input.Current.HasKeyboardFocus){throw 'Custom did not focus duration'}
}
elseif($action -eq 'focus-cancel'){
$condition=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::AutomationIdProperty,'cancel')
$root.FindFirst([System.Windows.Automation.TreeScope]::Descendants,$condition).SetFocus()
}
elseif($action -eq 'cancel'){
$input.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern).SetValue('2h')
$condition=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::AutomationIdProperty,'cancel')
$root.FindFirst([System.Windows.Automation.TreeScope]::Descendants,$condition).GetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern).Invoke()
for($i=0;$i -lt 40 -and $input.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern).Current.Value -ne '';$i++){Start-Sleep -Milliseconds 50}
if($input.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern).Current.Value -ne ''){throw 'Cancel retained draft'}
}
else{
$value=$input.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern)
$value.SetValue($action)
$condition=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::NameProperty,'Confirm duration')
$button=$root.FindFirst([System.Windows.Automation.TreeScope]::Descendants,$condition)
$invoke=$button.GetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern);$invoke.Invoke()
}
`, hwnd, action)
	encoded := base64.StdEncoding.EncodeToString(utf16Bytes(script))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encoded).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture accessibility %q: %v\n%s", action, err, out)
	}
}

func utf16Bytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, value := range units {
		out[2*i] = byte(value)
		out[2*i+1] = byte(value >> 8)
	}
	return out
}

func trayEventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(description)
}

func trayMenuLabel(menu, index uintptr) string {
	var text [256]uint16
	trayCall("GetMenuStringW", menu, index, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)), 0x400)
	return syscall.UTF16ToString(text[:])
}

func trayMenuItemID(menu uintptr, label string) uintptr {
	for index := uintptr(0); index < 32; index++ {
		if trayMenuLabel(menu, index) != label {
			continue
		}
		id := trayCall("GetMenuItemID", menu, index)
		if id != ^uintptr(0) && id != 0xffffffff {
			return id
		}
	}
	return ^uintptr(0)
}

const nativeLimitationRows = 8

func trayMenuContainsLimitations(menu uintptr, warnings []string) bool {
	for _, warning := range warnings {
		found := false
		for index := uintptr(10); index < 10+nativeLimitationRows; index++ {
			if strings.Contains(trayMenuLabel(menu, index), warning) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestNativeTrayShellKeyboardSelectionOpensMenu(t *testing.T) {
	requireNative(t)
	exe := os.Getenv("SNOOZAP_TRAY_EXE")
	if exe == "" {
		t.Skip("SNOOZAP_TRAY_EXE must name the built native tray")
	}
	if !filepath.IsAbs(exe) {
		t.Fatal("SNOOZAP_TRAY_EXE must be absolute")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "LOCALAPPDATA=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LOCALAPPDATA="+t.TempDir())
	log, err := os.Create(filepath.Join(t.TempDir(), "tray-keyselect.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	var hwnd uintptr
	trayEventually(t, "fixture tray window unavailable", func() bool {
		hwnd = trayWindow(cmd.Process.Pid, ^uintptr(2), "SnooZapTrayWindow")
		return hwnd != 0
	})
	// NIN_KEYSELECT is delivered as the low word of the tray callback. It is
	// the shell's ENTER/SPACE activation path, distinct from mouse callbacks.
	if trayCall("PostMessageW", hwnd, 0x401, 0, 0x401) == 0 {
		t.Fatal("fixture keyboard-selection callback failed")
	}
	var popup, menu uintptr
	trayEventually(t, "shell keyboard selection did not open the native menu", func() bool {
		popup = trayWindow(cmd.Process.Pid, 0, "#32768")
		if popup != 0 {
			menu = trayCall("SendMessageW", popup, 0x1e1, 0, 0)
		}
		return menu != 0 && strings.Contains(trayMenuLabel(menu, 0), "SnooZap")
	})
	quit := trayMenuItemID(menu, "Quit SnooZap")
	if quit == ^uintptr(0) || quit == 0xffffffff {
		t.Fatal("native Quit command not found after keyboard activation")
	}
	trayCall("PostMessageW", hwnd, 0x1f, 0, 0)
	trayCall("PostMessageW", hwnd, 0x111, quit, 0)
	if err := cmd.Wait(); err != nil {
		t.Fatal("native Quit after keyboard activation failed", err)
	}
}

func TestNativeTrayProcessMenuActivationAndQuit(t *testing.T) {
	requireNative(t)
	exe := os.Getenv("SNOOZAP_TRAY_EXE")
	if exe == "" {
		t.Skip("SNOOZAP_TRAY_EXE must name the built native tray")
	}
	if !filepath.IsAbs(exe) {
		t.Fatal("SNOOZAP_TRAY_EXE must be absolute")
	}
	for _, attached := range []bool{false, true} {
		name := "tray-owned"
		if attached {
			name = "console-owned"
		}
		t.Run(name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			previousDPI := trayCall("SetThreadDpiAwarenessContext", ^uintptr(3)) // Per-monitor V2, like Wails.
			if previousDPI == 0 {
				t.Fatal("fixture DPI context could not be set")
			}
			defer trayCall("SetThreadDpiAwarenessContext", previousDPI)
			var console *consoleProcess
			if attached {
				console = launch(t, executable(t), t.TempDir())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(entry), "LOCALAPPDATA=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "LOCALAPPDATA="+t.TempDir())
			log, err := os.Create(filepath.Join(t.TempDir(), "tray.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					foreground := trayCall("GetForegroundWindow")
					var foregroundPID uint32
					trayCall("GetWindowThreadProcessId", foreground, uintptr(unsafe.Pointer(&foregroundPID)))
					t.Logf("foreground=%x pid=%d fixture=%d", foreground, foregroundPID, cmd.Process.Pid)
					contents, _ := os.ReadFile(log.Name())
					t.Logf("fixture log:\n%s", contents)
				}
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			var hwnd uintptr
			trayEventually(t, "fixture tray window unavailable", func() bool {
				hwnd = trayWindow(cmd.Process.Pid, ^uintptr(2), "SnooZapTrayWindow")
				return hwnd != 0
			})
			client := ipc.NewClient(ipc.Dial)
			defer client.Close(context.Background())
			status := func(mode model.Mode) model.Snapshot {
				var s model.Snapshot
				trayEventually(t, "native menu did not apply "+string(mode), func() bool {
					var err error
					s, err = client.Snapshot(ctx)
					return err == nil && s.Mode == mode
				})
				return s
			}
			if s := status(model.Auto); s.NativeRequestActive || s.ConfigurationError != "" {
				t.Fatal("empty tray startup overclaimed or failed", s)
			}
			open := func(event uintptr) (uintptr, uintptr) {
				t.Logf("opening fixture menu callback=%x process=%d tray=%x", event, cmd.Process.Pid, hwnd)
				if trayCall("PostMessageW", hwnd, 0x401, 0, event) == 0 {
					t.Fatal("fixture tray callback failed")
				}
				var popup, menu uintptr
				trayEventually(t, "fixture native popup unavailable", func() bool {
					popup = trayWindow(cmd.Process.Pid, 0, "#32768")
					if popup != 0 {
						menu = trayCall("SendMessageW", popup, 0x1e1, 0, 0)
					}
					return menu != 0
				})
				return popup, menu
			}
			var statusWindow uintptr
			trayEventually(t, "bundled status window unavailable", func() bool {
				statusWindow = trayStatusWindow(cmd.Process.Pid)
				return statusWindow != 0
			})
			if trayCall("IsWindowVisible", statusWindow) != 0 {
				t.Fatal("tray startup unexpectedly showed the window")
			}
			trayCall("PostMessageW", hwnd, 0x401, 0, 0x400) // NIN_SELECT, primary activation.
			trayEventually(t, "primary activation did not show status", func() bool { return trayCall("IsWindowVisible", statusWindow) != 0 })
			trayCall("PostMessageW", statusWindow, 0x10, 0, 0)
			trayEventually(t, "close did not hide status", func() bool { return trayCall("IsWindowVisible", statusWindow) == 0 })
			if status(model.Auto).Mode != model.Auto {
				t.Fatal("window close changed mode")
			}
			popup, menu := open(0x205) // The shell's secondary-button callback.
			trayEventually(t, "native menu does not reflect the empty engine snapshot", func() bool {
				return strings.Contains(trayMenuLabel(menu, 0), "Normal sleep allowed") && trayMenuLabel(menu, 5) == "Auto" &&
					trayMenuContainsLimitations(menu, runtimeLimitations()[:3])
			})
			// Navigate by the menu's actual highlighted row. Windows may include
			// disabled status rows in keyboard navigation.
			for i := 0; i < 16; i++ {
				trayCall("PostMessageW", popup, 0x100, 0x28, 1)
				time.Sleep(30 * time.Millisecond)
				state := trayCall("GetMenuState", menu, 6, 0x400)
				if state&0x80 != 0 {
					trayCall("PostMessageW", popup, 0x100, 0x0d, 1)
					break
				}
				if i == 15 {
					t.Fatalf("keyboard did not highlight Stay awake: state=%x foreground=%x popup=%x owner=%x", state, trayCall("GetForegroundWindow"), popup, hwnd)
				}
			}
			if s := status(model.Awake); !s.NativeRequestAccepted {
				t.Fatal("native menu selected awake without accepted protection", s)
			}
			second := exec.CommandContext(ctx, exe, "mode", "allow-sleep")
			second.Env = cmd.Env
			if out, err := second.CombinedOutput(); err != nil {
				t.Fatal("repeated tray launch failed", err, string(out))
			}
			if s := status(model.Awake); !s.NativeRequestAccepted {
				t.Fatal("activation payload mutated or stopped the runtime", s)
			}
			trayEventually(t, "activation did not reopen the existing window", func() bool {
				return trayCall("IsWindowVisible", statusWindow) != 0
			})
			// This launcher is a background test process. Windows can deny its
			// foreground handoff while the user works elsewhere. Verify native
			// keyboard focus through the real accessibility provider separately.
			statusAccessibility(t, statusWindow, "focus")
			if trayStatusWindow(cmd.Process.Pid) != statusWindow {
				t.Fatal("activation replaced the existing window")
			}
			statusAccessibility(t, statusWindow, "focus-cancel")
			trayCall("PostMessageW", statusWindow, 0x10, 0, 0)
			trayEventually(t, "status window did not hide before Open SnooZap", func() bool {
				return trayCall("IsWindowVisible", statusWindow) == 0
			})
			_, menu = open(0x205)
			if !strings.Contains(trayMenuLabel(menu, 0), "Keeping awake") {
				t.Fatal("native view did not show accepted protection", trayMenuLabel(menu, 0))
			}
			invoke := func(current, index uintptr) {
				id := trayCall("GetMenuItemID", current, index)
				if id == ^uintptr(0) || id == 0xffffffff {
					t.Fatal("native command not found")
				}
				trayCall("PostMessageW", hwnd, 0x1f, 0, 0) // Cancel only this fixture's popup.
				trayCall("PostMessageW", hwnd, 0x111, id, 0)
			}
			openID := trayMenuItemID(menu, "Open SnooZap")
			if openID == ^uintptr(0) {
				t.Fatal("Open SnooZap action missing")
			}
			trayCall("PostMessageW", hwnd, 0x1f, 0, 0)
			trayCall("PostMessageW", hwnd, 0x111, openID, 0)
			trayEventually(t, "Open SnooZap did not show the existing window", func() bool {
				return trayCall("IsWindowVisible", statusWindow) != 0 && trayStatusWindow(cmd.Process.Pid) == statusWindow
			})
			trayCall("PostMessageW", statusWindow, 0x10, 0, 0)
			trayEventually(t, "Open SnooZap close did not hide", func() bool { return trayCall("IsWindowVisible", statusWindow) == 0 })
			_, menu = open(0x205)
			presets := trayCall("GetSubMenu", menu, 7)
			if trayMenuLabel(presets, 0) != "15 minutes" || trayMenuLabel(presets, 2) != "4 hours" {
				t.Fatal("timed native menu presets missing")
			}
			if trayMenuLabel(presets, 3) != "Custom…" {
				t.Fatal("Custom action missing")
			}
			invoke(presets, 3)
			trayEventually(t, "Custom did not open the status window", func() bool { return trayCall("IsWindowVisible", statusWindow) != 0 })
			statusAccessibility(t, statusWindow, "check-focus")
			statusAccessibility(t, statusWindow, "1h30m")
			var custom model.Snapshot
			trayEventually(t, "graphical Custom did not reach shared Service", func() bool {
				var err error
				custom, err = client.Snapshot(ctx)
				return err == nil && custom.Deadline != nil && custom.RemainingMS > 89*time.Minute.Milliseconds() && custom.NativeRequestAccepted
			})
			statusAccessibility(t, statusWindow, "cancel")
			statusAccessibility(t, statusWindow, "invalid")
			statusAccessibility(t, statusWindow, "check-focus")
			unchanged, err := client.Snapshot(ctx)
			if err != nil || unchanged.Deadline == nil || !unchanged.Deadline.Equal(*custom.Deadline) {
				t.Fatal("invalid/cancel changed custom timer", err)
			}
			if console != nil {
				if fromConsole := console.status(t, ""); fromConsole.Deadline == nil || !fromConsole.Deadline.Equal(*custom.Deadline) {
					t.Fatal("attached console disagrees with graphical Custom")
				}
			}
			trayCall("PostMessageW", statusWindow, 0x10, 0, 0)
			trayEventually(t, "Custom close did not hide", func() bool { return trayCall("IsWindowVisible", statusWindow) == 0 })
			preserved, err := client.Snapshot(ctx)
			if err != nil || preserved.Deadline == nil || !preserved.Deadline.Equal(*custom.Deadline) || !preserved.NativeRequestAccepted {
				t.Fatal("closing Custom lost timer/protection", err)
			}
			_, menu = open(0x205)
			presets = trayCall("GetSubMenu", menu, 7)
			invoke(presets, 1)
			trayEventually(t, "native timed action did not set its deadline", func() bool {
				s, err := client.Snapshot(ctx)
				return err == nil && s.Deadline != nil && s.RemainingMS > 59*time.Minute.Milliseconds() && s.RemainingMS <= time.Hour.Milliseconds()
			})
			popup, menu = open(0x7b) // The shell's context-menu callback.
			trayEventually(t, "native Allow sleep row remains disabled", func() bool {
				return trayCall("GetMenuState", menu, 8, 0x400)&3 == 0
			})
			if trayCall("HiliteMenuItem", hwnd, menu, 6, 0x480) == 0 {
				t.Fatal("native timed menu could not highlight a mode")
			}
			previous, updates := trayMenuLabel(menu, 2), 0
			trayEventually(t, "native menu did not render three countdown changes", func() bool {
				if trayCall("GetMenuState", menu, 6, 0x400)&0x80 == 0 {
					t.Fatal("countdown updates cleared the native keyboard highlight")
				}
				if current := trayMenuLabel(menu, 2); current != previous {
					previous = current
					updates++
				}
				return updates >= 3
			})
			var rect struct{ Left, Top, Right, Bottom int32 }
			if trayCall("GetMenuItemRect", 0, menu, 8, uintptr(unsafe.Pointer(&rect))) == 0 {
				t.Fatal("native Allow sleep row has no mouse target")
			}
			point := struct{ X, Y int32 }{(rect.Left + rect.Right) / 2, (rect.Top + rect.Bottom) / 2}
			var saved struct{ X, Y int32 }
			if trayCall("GetCursorPos", uintptr(unsafe.Pointer(&saved))) == 0 {
				t.Fatal("could not save the mouse position")
			}
			defer func() {
				var current struct{ X, Y int32 }
				if trayCall("GetCursorPos", uintptr(unsafe.Pointer(&current))) != 0 && current == point {
					trayCall("SetCursorPos", uintptr(saved.X), uintptr(saved.Y))
				}
			}()
			trayCall("SetCursorPos", uintptr(point.X), uintptr(point.Y))
			// TrackPopupMenu uses the real cursor position. Before clicking,
			// prove the point still belongs to this exact fixture process.
			target := trayCall("WindowFromPoint", uintptr(uint32(point.X))|uintptr(uint32(point.Y))<<32)
			var targetPID uint32
			trayCall("GetWindowThreadProcessId", target, uintptr(unsafe.Pointer(&targetPID)))
			if int(targetPID) != cmd.Process.Pid || target != popup {
				t.Fatalf("mouse target differs: target=%x pid=%d popup=%x fixture=%d rect=%+v point=%+v", target, targetPID, popup, cmd.Process.Pid, rect, point)
			}
			trayCall("mouse_event", 0x2, 0, 0, 0, 0)
			trayCall("mouse_event", 0x4, 0, 0, 0, 0)
			if s := status(model.AllowSleep); s.NativeRequestActive || s.Deadline != nil {
				t.Fatal("Allow sleep retained the timer or native request", s)
			}
			if console != nil && console.status(t, "").Mode != model.AllowSleep {
				t.Fatal("console disagrees with native menu")
			}
			_, menu = open(0x205)
			quit := trayMenuItemID(menu, "Quit SnooZap")
			if quit == ^uintptr(0) || quit == 0xffffffff {
				t.Fatal("native Quit command not found")
			}
			trayCall("PostMessageW", hwnd, 0x1f, 0, 0)
			trayCall("PostMessageW", hwnd, 0x111, quit, 0)
			if err := cmd.Wait(); err != nil {
				t.Fatal("native Quit failed", err)
			}
			if console != nil {
				console.stdin.Close()
				if err := console.cmd.Wait(); err != nil {
					t.Fatal("tray Quit did not stop the attached owner", err)
				}
				console.cancel()
			}
			owner, err := native.AcquireOwner()
			if err != nil {
				t.Fatal("native tray Quit retained engine ownership", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTrayControllerBatteryCutoffAndTelemetryFailure(t *testing.T) {
	var unavailable atomic.Bool
	percent := 10
	events := make(chan model.LifecycleEvent, 1)
	service := engine.New(context.Background(), &trayPower{}, nil, "", engine.Options{
		Events: events,
		ReadPower: func(context.Context) (model.Battery, error) {
			if unavailable.Load() {
				return model.Battery{}, errors.New("fixture telemetry failure")
			}
			return model.Battery{Known: true, Source: "battery", Present: true, Percent: &percent}, nil
		},
	})
	updates, update := trayUpdates()
	c := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Execute(model.Command{Operation: "mode", Mode: model.Awake}); err != nil {
		t.Fatal(err)
	}
	s := waitTray(t, updates, func(s desktop.State) bool { return !s.Pending && s.Snapshot.Battery.Blocked })
	if s.Snapshot.Mode != model.Awake || s.Snapshot.NativeRequestActive || desktop.Describe(s).Title != "Sleep prevention paused" {
		t.Fatal("battery guard did not reach the tray truthfully", s)
	}
	unavailable.Store(true)
	events <- model.PowerChanged
	s = waitTray(t, updates, func(s desktop.State) bool { return !s.Snapshot.Battery.Known && s.Snapshot.Battery.Blocked })
	if s.Snapshot.NativeRequestAccepted || desktop.Describe(s).Icon != "paused" {
		t.Fatal("telemetry failure overclaimed protection", s)
	}
}
