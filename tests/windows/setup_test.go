//go:build windows

package windows_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omry/snoozap/frontend"
	"github.com/omry/snoozap/internal/config"
	"github.com/omry/snoozap/internal/desktop"
	"github.com/omry/snoozap/internal/engine"
	"github.com/omry/snoozap/internal/history"
	"github.com/omry/snoozap/internal/model"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

const setupConfiguration = `{"schema_version":1,"probes":{"http":{"type":"http","enabled":false,"url":"http://127.0.0.1:8080/private","pointer":"/count","operator":"equals","value":9007199254740993.125,"credential_headers":{"Authorization":"opaque-ref"}},"app":{"type":"process","enabled":false,"name":"Editor.exe","on_error":"release_after","on_startup_unknown":"hold"},"t3":{"type":"t3","enabled":false,"local_environment":"existing"}},"rules":[{"id":"nested","enabled":false,"when":{"all":[{"probe":"http"},{"not":{"any":[{"probe":"app"},{"probe":"t3"}]}}]}}],"settings":{"monitoring":true,"battery":{"ac_only":false,"floor_percent":20,"resume_percent":25}}}`

func setupConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(setupConfiguration))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func setupBridge(t *testing.T, options engine.Options) (*desktop.StatusBridge, *desktop.Controller, *engine.Engine) {
	t.Helper()
	service := engine.New(context.Background(), &trayPower{}, nil, "", options)
	updates, update := trayUpdates()
	controller := desktop.NewController(context.Background(), service, update)
	t.Cleanup(func() { _ = controller.Close() })
	waitTray(t, updates, func(s desktop.State) bool { return s.Connected })
	return desktop.NewStatusBridge(controller), controller, service
}

func TestSetupFullCandidatePreservesExactConfigAndManualDeadline(t *testing.T) {
	cfg := setupConfig(t)
	var saved []byte
	store := config.Store{Replace: func(data []byte) error { saved = append([]byte(nil), data...); return nil }}
	bridge, _, service := setupBridge(t, engine.Options{Configuration: cfg, Save: store.Save, ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }})
	manual, err := bridge.Mode(model.Awake, "1h")
	if err != nil || manual.State.Snapshot.Deadline == nil || !manual.State.Snapshot.NativeRequestAccepted {
		t.Fatal(manual, err)
	}
	doc, err := bridge.SetupRead()
	if err != nil || doc.Comparisons["http"] != "9007199254740993.125" {
		t.Fatal(doc, err)
	}
	loaded, err := config.Parse(strings.NewReader(doc.Configuration))
	if err != nil || !reflect.DeepEqual(loaded, cfg) {
		t.Fatal("read changed config", err)
	}
	loaded.Settings.Monitoring = false
	loaded.Settings.Battery.FloorPercent = 30
	loaded.Settings.Battery.ResumePercent = 40
	data, _ := config.Export(loaded)
	view, err := bridge.SetupApply(string(data))
	if err != nil || view.State.Snapshot.Monitoring || view.State.Snapshot.Mode != model.Awake || !view.State.Snapshot.Deadline.Equal(*manual.State.Snapshot.Deadline) || !view.State.Snapshot.NativeRequestAccepted {
		t.Fatal("apply changed manual mode/deadline or did not apply monitoring", view, err)
	}
	persisted, err := config.Parse(bytes.NewReader(saved))
	if err != nil || !reflect.DeepEqual(persisted, loaded) {
		t.Fatal("persisted candidate differs", err)
	}
	exported, _ := service.ExportConfig(context.Background())
	if !bytes.Equal(exported, saved) || !bytes.Contains(exported, []byte("9007199254740993.125")) {
		t.Fatal("active and saved config differ")
	}
}

func TestSetupInvalidOrUnpersistedCandidateKeepsOldConfigAndTimer(t *testing.T) {
	var writes int
	bridge, _, service := setupBridge(t, engine.Options{Configuration: setupConfig(t), Save: func(config.Config) error { writes++; return errors.New("private storage failure") }})
	manual, err := bridge.Mode(model.Awake, "1h")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := service.ExportConfig(context.Background())
	invalid := []string{
		strings.Replace(setupConfiguration, `"schema_version":1`, `"schema_version":2`, 1),
		strings.Replace(setupConfiguration, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		strings.Replace(setupConfiguration, `"/count"`, `"\ud800"`, 1),
		strings.Replace(setupConfiguration, `"probe":"http"`, `"probe":"missing"`, 1),
		strings.Replace(setupConfiguration, `"floor_percent":20`, `"floor_percent":30`, 1),
		strings.Replace(setupConfiguration, `"monitoring":true`, `"monitoring":null`, 1),
		strings.Replace(setupConfiguration, `"settings":`, `"unexpected":1,"settings":`, 1),
		strings.Repeat(" ", config.MaxBytes+1),
	}
	for i, candidate := range invalid {
		view, err := bridge.SetupApply(candidate)
		if err == nil || writes != 0 || view.State.Snapshot.Mode != model.Awake || !view.State.Snapshot.Deadline.Equal(*manual.State.Snapshot.Deadline) {
			t.Fatal("invalid candidate mutated owner", i, view, writes, err)
		}
	}
	view, err := bridge.SetupApply(strings.Replace(setupConfiguration, `"monitoring":true`, `"monitoring":false`, 1))
	if err == nil || writes != 1 || !view.State.Snapshot.Monitoring || !view.State.Snapshot.Deadline.Equal(*manual.State.Snapshot.Deadline) {
		t.Fatal("failed persistence changed active config/timer", view, err)
	}
	after, _ := service.ExportConfig(context.Background())
	if !bytes.Equal(before, after) {
		t.Fatal("rejected candidate changed config")
	}
}

func TestSetupMonitoringAndBatteryEffectsRemainEngineOwned(t *testing.T) {
	percent := 35
	bridge, _, _ := setupBridge(t, engine.Options{Configuration: config.Empty(), Save: func(config.Config) error { return nil }, ReadPower: func(context.Context) (model.Battery, error) {
		return model.Battery{Source: "battery", Present: true, Known: true, Percent: &percent}, nil
	}})
	manual, err := bridge.Mode(model.Awake, "1h")
	if err != nil || !manual.State.Snapshot.NativeRequestAccepted {
		t.Fatal(manual, err)
	}
	cfg := config.Empty()
	cfg.Settings.Monitoring = false
	cfg.Settings.Battery.FloorPercent = 40
	cfg.Settings.Battery.ResumePercent = 45
	data, _ := config.Export(cfg)
	view, err := bridge.SetupApply(string(data))
	if err != nil || view.State.Snapshot.Monitoring || view.State.Snapshot.NativeRequestActive || view.State.Snapshot.Mode != model.Awake || !view.State.Snapshot.Deadline.Equal(*manual.State.Snapshot.Deadline) {
		t.Fatal("battery suppression did not retain manual selection", view, err)
	}
	cfg.Settings.Battery.FloorPercent = 10
	cfg.Settings.Battery.ResumePercent = 15
	data, _ = config.Export(cfg)
	view, err = bridge.SetupApply(string(data))
	if err != nil || !view.State.Snapshot.NativeRequestAccepted || view.State.Snapshot.Monitoring {
		t.Fatal("monitoring off disabled manual hold", view, err)
	}
	view, err = bridge.Mode(model.AllowSleep, "")
	if err != nil || view.State.Snapshot.NativeRequestActive || view.State.Snapshot.Mode != model.AllowSleep || view.State.Snapshot.Deadline != nil {
		t.Fatal(view, err)
	}
}

func TestSetupHistoryUsesOwnerRedactionAndClearPreservesTimer(t *testing.T) {
	bridge, _, service := setupBridge(t, engine.Options{Configuration: setupConfig(t), Save: func(config.Config) error { return nil }})
	manual, _ := bridge.Mode(model.Awake, "1h")
	if _, err := bridge.SetupApply(setupConfiguration); err != nil {
		t.Fatal(err)
	}
	before, _ := service.Snapshot(context.Background())
	h, err := bridge.HistoryRead()
	if err != nil || len(h.Records) == 0 || len(h.Records) > 2*history.Limit {
		t.Fatal(h, err)
	}
	data, _ := json.Marshal(h)
	for _, secret := range []string{"opaque-ref", "127.0.0.1", "/private", "Authorization", "9007199254740993"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("history leaked config", secret)
		}
	}
	if cleared, err := bridge.HistoryClear(); err != nil || len(cleared.Records) != 0 {
		t.Fatal("clear failed", cleared, err)
	}
	after, _ := service.Snapshot(context.Background())
	if before.Sequence != after.Sequence || after.Mode != model.Awake || !after.Deadline.Equal(*manual.State.Snapshot.Deadline) {
		t.Fatal("history operation changed owner", before, after)
	}
}

type setupFailingService struct{ model.Service }

func (setupFailingService) ExportConfig(context.Context) ([]byte, error) {
	return nil, errors.New("SECRET")
}
func (setupFailingService) History(context.Context) (history.Snapshot, error) {
	return history.Snapshot{}, errors.New("SECRET")
}
func (setupFailingService) ClearHistory(context.Context) error { return errors.New("SECRET") }

func TestSetupReadAndClearFailuresDoNotExposeRawErrors(t *testing.T) {
	service := engine.New(context.Background(), &trayPower{}, nil, "")
	controller := desktop.NewController(context.Background(), setupFailingService{service}, func(desktop.State) {})
	t.Cleanup(func() { _ = controller.Close() })
	bridge := desktop.NewStatusBridge(controller)
	_, e1 := bridge.SetupRead()
	_, e2 := bridge.HistoryRead()
	_, e3 := bridge.HistoryClear()
	for _, err := range []error{e1, e2, e3} {
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("raw service error leaked", err)
		}
	}
}

type setupCorruptExportService struct{ model.Service }

func (setupCorruptExportService) ExportConfig(context.Context) ([]byte, error) {
	return []byte(`{"schema_version":99}`), nil
}

type setupAfterClearFailureService struct{ model.Service }

func (setupAfterClearFailureService) ClearHistory(context.Context) error { return nil }
func (setupAfterClearFailureService) History(context.Context) (history.Snapshot, error) {
	return history.Snapshot{}, errors.New("SECRET")
}

func TestSetupRejectsCorruptExportAndAnUnavailableReadAfterClear(t *testing.T) {
	for _, which := range []string{"corrupt-export", "after-clear"} {
		t.Run(which, func(t *testing.T) {
			var service model.Service = engine.New(context.Background(), &trayPower{}, nil, "")
			if which == "corrupt-export" {
				service = setupCorruptExportService{service}
			} else {
				service = setupAfterClearFailureService{service}
			}
			controller := desktop.NewController(context.Background(), service, func(desktop.State) {})
			t.Cleanup(func() { _ = controller.Close() })
			bridge := desktop.NewStatusBridge(controller)
			var err error
			if which == "corrupt-export" {
				_, err = bridge.SetupRead()
			} else {
				_, err = bridge.HistoryClear()
			}
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid response was accepted or leaked raw error", err)
			}
		})
	}
}

type setupBlockingClearService struct {
	model.Service
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (s *setupBlockingClearService) ClearHistory(ctx context.Context) error {
	s.calls.Add(1)
	close(s.entered)
	select {
	case <-s.release:
		return s.Service.ClearHistory(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSetupClearSerializesWithModesAndCancelsWithController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &setupBlockingClearService{Service: engine.New(ctx, &trayPower{}, nil, ""), entered: make(chan struct{}), release: make(chan struct{})}
	controller := desktop.NewController(ctx, service, func(desktop.State) {})
	t.Cleanup(func() { _ = controller.Close() })
	bridge := desktop.NewStatusBridge(controller)
	done := make(chan error, 1)
	go func() { _, err := bridge.HistoryClear(); done <- err }()
	select {
	case <-service.entered:
	case <-time.After(time.Second):
		t.Fatal("clear did not enter")
	}
	if _, err := bridge.Mode(model.Awake, "1h"); !errors.Is(err, model.ErrUnavailable) {
		t.Fatal("queued concurrent mode", err)
	}
	if _, err := bridge.HistoryClear(); !errors.Is(err, model.ErrUnavailable) || service.calls.Load() != 1 {
		t.Fatal("replayed concurrent clear", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled clear succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("clear ignored cancellation")
	}
}

func TestSetupGuardBindsOnlyExactMaintenanceOperations(t *testing.T) {
	for _, method := range []string{"SetupRead", "SetupApply", "HistoryRead", "HistoryClear"} {
		for _, origin := range []string{"http://wails.localhost", "null", "http://localhost", "https://evil.test"} {
			for _, arityOK := range []bool{true, false} {
				args := []any{}
				if method == "SetupApply" {
					args = append(args, setupConfiguration)
				}
				if !arityOK {
					args = append(args, "extra")
				}
				body := statusCall(method, args...)
				dispatched := false
				h := desktop.StatusMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { dispatched = true; w.WriteHeader(http.StatusNoContent) }))
				r := httptest.NewRequest("POST", "http://wails.localhost/wails/runtime", strings.NewReader(body))
				r.Header.Set("Origin", origin)
				r.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				h.ServeHTTP(out, r)
				if dispatched != (arityOK && origin == "http://wails.localhost") {
					t.Fatal(method, origin, arityOK, out.Code)
				}
			}
		}
	}
	for _, path := range []string{"/setup.js", "/rules.js", "/settings.js", "/history.js"} {
		out := httptest.NewRecorder()
		desktop.StatusMiddleware(frontend.Handler()).ServeHTTP(out, httptest.NewRequest("GET", "http://wails.localhost"+path, nil))
		if out.Code != 200 || !strings.Contains(out.Header().Get("Content-Type"), "javascript") {
			t.Fatal(path, out.Code)
		}
	}
	for _, body := range []string{statusCall("SetupApply", strings.Repeat("x", 2*config.MaxBytes+4096)), strings.Replace(statusCall("SetupApply", "x"), `"x"`, `"\ud800"`, 1)} {
		h := desktop.StatusMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("malformed/oversized dispatch") }))
		r := httptest.NewRequest("POST", "http://wails.localhost/wails/runtime", strings.NewReader(body))
		r.Header.Set("Origin", "http://wails.localhost")
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		if out.Code != 403 {
			t.Fatal(out.Code)
		}
	}
}

// This opt-in isolated WebView uses fake power and a private data directory. It
// drives the real bundled modules without mouse/keyboard injection or owner lock.
func TestNativeSetupBundledRulesSettingsHistory(t *testing.T) {
	requireNative(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestSetupWebviewFixtureChild$", "-test.v", "-test.timeout=35s")
	cmd.Env = append(os.Environ(), "SNOOZAP_SETUP_FIXTURE=1", "SNOOZAP_SETUP_FIXTURE_DATA="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("SETUP_NATIVE_PASSED")) {
		t.Fatalf("native maintenance UI: %v\n%s", err, out)
	}
	t.Log(string(out))
}

func TestSetupWebviewFixtureChild(t *testing.T) {
	if os.Getenv("SNOOZAP_SETUP_FIXTURE") != "1" {
		t.Skip("isolated setup fixture child only")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr := w32.CoInitializeEx(w32.COINIT_APARTMENTTHREADED); hr != 0 && hr != 1 {
		t.Fatalf("COM: %x", hr)
	}
	defer w32.CoUninitialize()
	var saves atomic.Int32
	bridge, _, service := setupBridge(t, engine.Options{Configuration: setupConfig(t), Save: func(cfg config.Config) error {
		data, err := config.Export(cfg)
		if err == nil {
			err = os.WriteFile(filepath.Join(os.Getenv("SNOOZAP_SETUP_FIXTURE_DATA"), "config.json"), data, 0600)
		}
		if err == nil {
			saves.Add(1)
		}
		return err
	}, ReadPower: func(context.Context) (model.Battery, error) { return model.Battery{Source: "ac", Known: true}, nil }})
	results := make(chan string, 2)
	assets := frontend.Handler()
	a := application.New(application.Options{Name: "SnooZap isolated maintenance acceptance", Services: []application.Service{application.NewService(bridge)},
		Windows: application.WindowsOptions{WndClass: "SnooZapIsolatedSetupFixture", WebviewUserDataPath: os.Getenv("SNOOZAP_SETUP_FIXTURE_DATA")},
		Assets: application.AssetOptions{Middleware: func(next http.Handler) http.Handler {
			return desktop.StatusMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wails/runtime" {
					data, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(data))
					var call struct {
						Args struct {
							ID string `json:"call-id"`
						} `json:"args"`
					}
					_ = json.Unmarshal(data, &call)
					if strings.HasPrefix(call.Args.ID, "setup-fixture-") {
						results <- call.Args.ID
					}
				}
				next.ServeHTTP(w, r)
			}))
		}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/setup.js" {
				assets.ServeHTTP(w, r)
				return
			}
			out := httptest.NewRecorder()
			assets.ServeHTTP(out, r)
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(out.Body.Bytes())
			_, _ = io.WriteString(w, "\n"+setupNativeDriver)
		})},
	})
	a.Window.NewWithOptions(application.WebviewWindowOptions{Title: "SnooZap isolated maintenance acceptance", URL: "/", Width: 620, Height: 720, Hidden: true, DevToolsEnabled: false})
	completed := make(chan error, 1)
	go func() {
		var failure error
		select {
		case result := <-results:
			if result != "setup-fixture-passed" {
				failure = errors.New(result)
			}
		case <-time.After(25 * time.Second):
			failure = errors.New("setup fixture timed out")
		}
		if failure == nil && saves.Load() != 1 {
			failure = fmt.Errorf("expected one config save, got%d", saves.Load())
		}
		if failure == nil {
			data, err := service.ExportConfig(context.Background())
			if err != nil || !bytes.Contains(data, []byte("9007199254740993.125")) {
				failure = errors.New("exact scalar changed")
			}
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
	t.Log("SETUP_NATIVE_PASSED: bundled rules/settings/history; one full candidate; exact decimal; retained timer; rejected invalid candidate; fake power only")
}

const setupNativeDriver = `
(async()=>{
const wait=async(test)=>{for(let i=0;i<150;i++){if(test())return;await new Promise(r=>setTimeout(r,40))}throw Error('UI wait failed')};
const call=async(name,args=[],id='native-setup')=>{const r=await fetch('/wails/runtime',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({object:0,method:0,args:{'call-id':id,methodName:'github.com/omry/snoozap/internal/desktop.StatusBridge.'+name,args}})});if(!r.ok)throw Error('binding '+r.status);return r.json()};
const doc=document;await wait(()=>!doc.getElementById('save-configuration').disabled);
await call('Mode',['awake','1h']);const before=await call('Read');
const control=[...doc.querySelectorAll('label')].find(n=>n.firstChild.textContent==='Rule ID').querySelector('input');
control.value='native_draft';control.dispatchEvent(new Event('input'));control.focus();control.setSelectionRange(1,4);
await new Promise(r=>setTimeout(r,2200));if(doc.activeElement!==control||control.value!=='native_draft'||control.selectionStart!==1)throw Error('poll clobbered draft/focus');
doc.querySelector('[data-view="settings"]').click();const monitor=[...doc.querySelectorAll('label')].find(n=>n.firstChild.textContent==='Monitor rules').querySelector('input');
monitor.checked=false;monitor.dispatchEvent(new Event('input'));doc.getElementById('save-configuration').click();await wait(()=>doc.getElementById('setup-message').textContent.includes('Configuration saved'));
const after=await call('Read');if(after.State.Snapshot.mode!=='awake'||after.State.Snapshot.deadline!==before.State.Snapshot.deadline||after.State.Snapshot.monitoring)throw Error('config/timer mismatch');
doc.querySelector('[data-view="rules"]').click();control.value='';control.dispatchEvent(new Event('input'));doc.getElementById('save-configuration').click();await wait(()=>doc.getElementById('setup-message').textContent.includes('not confirmed'));
if((await call('Read')).State.Snapshot.deadline!==before.State.Snapshot.deadline||control.value!=='')throw Error('reject changed timer/draft');
doc.querySelector('[data-view="history"]').click();await wait(()=>doc.querySelectorAll('#history-panel tbody tr').length>0);
let exported,downloaded=false;const original=URL.createObjectURL;URL.createObjectURL=blob=>{exported=blob;return original.call(URL,blob)};
const click=HTMLAnchorElement.prototype.click;HTMLAnchorElement.prototype.click=function(){const event=new MouseEvent('click',{bubbles:true,cancelable:true});this.dispatchEvent(event);downloaded=this.download==='snoozap-history.json'&&this.href.startsWith('blob:')&&!event.defaultPrevented};
[...doc.querySelectorAll('#history-panel button')].find(n=>n.textContent==='Export history').click();await wait(()=>downloaded);
HTMLAnchorElement.prototype.click=click;const data=await exported.text();if(data.includes('opaque-ref')||data.includes('127.0.0.1')||data.includes('/private')||!JSON.parse(data).records.length)throw Error('unsafe export');
[...doc.querySelectorAll('#history-panel button')].find(n=>n.textContent==='Clear history').click();await wait(()=>doc.querySelector('#history-panel [role="status"]').textContent==='0 records after clear.');
if((await call('Read')).State.Snapshot.deadline!==before.State.Snapshot.deadline)throw Error('history changed timer');
await call('Read',[],'setup-fixture-passed');
})().catch(async error=>{await fetch('/wails/runtime',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({object:0,method:0,args:{'call-id':'setup-fixture-failed-'+String(error).slice(0,35),methodName:'github.com/omry/snoozap/internal/desktop.StatusBridge.Read',args:[]}})})});
`
