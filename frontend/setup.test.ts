import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { mountSetup, readDraft, writeDraft } from "./dist/setup.js";
import { mount } from "./dist/status.js";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const tick = () => new Promise(resolve => setImmediate(resolve));
function documentValue() {
  return { configuration: '{"schema_version":1,"probes":{"http":{"type":"http","enabled":true,"on_startup_unknown":"wait","on_error":"retain","grace_seconds":60,"interval_ms":5000,"timeout_ms":2000,"url":"http://localhost:8080/","pointer":"/count","operator":"equals","value":9007199254740993.125,"credential_headers":{"Authorization":"opaque-ref"}}},"rules":[{"id":"keep","enabled":true,"when":{"not":{"any":[{"probe":"http"}]}}}],"settings":{"monitoring":true,"battery":{"ac_only":false,"floor_percent":20,"resume_percent":25}}}', comparisons: { http: "9007199254740993.125" }, configuration_revision: 1 };
}
function fixture(t, handler = async (name, args) => name === "SetupRead" ? documentValue() : name === "SetupApply" ? { State: { Snapshot: { configuration_revision: 2 } } } : {}) {
  const dom = new JSDOM(html, { url: "http://wails.localhost/", pretendToBeVisual: true }), calls = [];
  const fetcher = async (url, init) => {
    const body = JSON.parse(init.body); calls.push({ url, init, name: body.args.methodName.split(".").at(-1), args: body.args.args });
    return new Response(JSON.stringify(await handler(calls.at(-1).name, body.args.args)));
  };
  const downloads = [];
  const ui = mountSetup(dom.window.document, fetcher, dom.window.location.origin, data => downloads.push(data));
  t.after(() => { ui.stop(); dom.window.close(); });
  return { dom, doc: dom.window.document, calls, ui, fetcher, downloads };
}
function input(f, text, value) {
  const label = [...f.doc.querySelectorAll("label")].find(label => label.firstChild?.textContent === text);
  assert.ok(label, text); const control = label.querySelector("input,select");
  if (control.type === "checkbox") control.checked = value; else control.value = value;
  control.dispatchEvent(new f.dom.window.Event(control.tagName === "SELECT" ? "change" : "input", { bubbles: true }));
  return control;
}
test("raw comparison decimals and every unrelated config field round-trip exactly", () => {
  const initial = documentValue(), draft = readDraft(initial);
  assert.equal(draft.probes.http.value, "9007199254740993.125");
  const original = draft.rules; draft.settings.monitoring = false;
  const data = writeDraft(draft);
  assert.match(data, /"value":9007199254740993\.125/);
  const parsed = JSON.parse(data); assert.equal(parsed.settings.monitoring, false);
  assert.deepEqual(parsed.rules, original); assert.deepEqual(parsed.probes.http.credential_headers, { Authorization: "opaque-ref" });
  for (const raw of ["1e999", "-0", "null", '"😀"', '"\\ufffd"', "true", "123456789012345678901234567890"]) {
    draft.probes.http.value = raw; assert.ok(writeDraft(draft).includes(`"value":${raw}`));
  }
  for (const raw of ["[]", "{}", "true,false", '"unterminated']) { draft.probes.http.value = raw; assert.throws(() => writeDraft(draft)); }
  assert.throws(() => readDraft({ ...initial, comparisons: {} }), /Missing exact/);
  draft.probes.http.value = "true"; draft.probes.http.url = "x".repeat(1048576); assert.throws(() => writeDraft(draft), /1 MiB/);
});
test("configuration uses exact-origin bounded fixed calls and one explicit full candidate apply", async t => {
  const f = fixture(t); await tick();
  assert.equal(f.calls.length, 1); assert.equal(f.calls[0].name, "SetupRead");
  assert.equal(f.calls[0].url, "/wails/runtime"); assert.equal(f.calls[0].init.method, "POST");
  assert.equal(f.calls[0].init.credentials, "omit"); assert.equal(f.calls[0].init.redirect, "error");
  input(f, "Rule ID", "edited");
  const form = f.doc.getElementById("configuration");
  form.dispatchEvent(new f.dom.window.Event("submit", { cancelable: true }));
  form.dispatchEvent(new f.dom.window.Event("submit", { cancelable: true }));
  await tick(); await tick();
  const mutations = f.calls.filter(call => call.name === "SetupApply"); assert.equal(mutations.length, 1);
  assert.equal(JSON.parse(mutations[0].args[0]).rules[0].id, "edited");
  assert.match(mutations[0].args[0], /9007199254740993\.125/);
  assert.match(f.doc.getElementById("setup-message").textContent, /Configuration saved/);
});
test("a successful save advances the draft revision for the next edit", async t => {
  let revision = 1;
  const f = fixture(t, async (name) => name === "SetupRead" ? documentValue() : name === "SetupApply" ? { State: { Snapshot: { configuration_revision: ++revision } } } : {});
  await tick();
  input(f, "Rule ID", "first"); f.doc.getElementById("save-configuration").click(); await tick(); await tick();
  input(f, "Rule ID", "second"); f.doc.getElementById("save-configuration").click(); await tick(); await tick();
  const mutations = f.calls.filter(call => call.name === "SetupApply");
  assert.equal(mutations.length, 2);
  assert.equal(mutations[0].args[1], 1);
  assert.equal(mutations[1].args[1], 2);
  assert.match(f.doc.getElementById("setup-message").textContent, /Configuration saved/);
});
test("invalid scalar prevents dispatch and failed or uncertain saves retain all drafts without replay", async t => {
  const f = fixture(t, async name => { if (name === "SetupRead") return documentValue(); throw Error("private backend error"); });
  await tick(); input(f, "Expected JSON scalar (for example true, 42 or \"ready\")", "[]");
  f.doc.getElementById("save-configuration").click(); await tick();
  assert.equal(f.calls.filter(call => call.name === "SetupApply").length, 0);
  input(f, "Expected JSON scalar (for example true, 42 or \"ready\")", "42"); input(f, "Rule ID", "my-draft");
  f.doc.getElementById("save-configuration").click(); await tick(); await tick();
  assert.equal(f.calls.filter(call => call.name === "SetupApply").length, 1);
  assert.equal(f.doc.querySelector('label input[type="text"]').value, "my-draft");
  assert.match(f.doc.getElementById("setup-message").textContent, /Save not confirmed/);
  assert.doesNotMatch(f.doc.body.textContent, /private backend/);
  await tick(); assert.equal(f.calls.filter(call => call.name === "SetupApply").length, 1);
});
test("status polling and history activity preserve configuration DOM, selection and focus", async t => {
  const f = fixture(t, async name => name === "SetupRead" ? documentValue() : name === "Read" ? {
    State: { Connected: true, Pending: false, Snapshot: { sequence: 1, mode: "awake", activity: "active" } },
    Presentation: { Title: "Keeping awake", Reason: "Manual timer", Selected: "1h", Request: "Accepted", Icon: "accepted", Limitations: [] },
  } : { schema_version: 1, records: [] });
  const status = mount(f.doc, f.fetcher, f.dom.window.location.origin); t.after(() => status.stop());
  await tick(); const control = input(f, "Rule ID", "draft-typing"); control.focus(); control.setSelectionRange(2, 5);
  for (let i = 0; i < 4; i++) await status.refresh();
  assert.equal(f.doc.activeElement, control); assert.equal(control.value, "draft-typing");
  assert.deepEqual([control.selectionStart, control.selectionEnd], [2, 5]);
  f.doc.querySelector('[data-view="history"]').click(); await tick();
  f.doc.querySelector('[data-view="rules"]').click();
  assert.equal(f.doc.querySelector('label input[type="text"]'), control);
  assert.equal(control.value, "draft-typing");
  assert.equal(f.calls.filter(call => call.name === "SetupApply").length, 0);
});
test("explicit reload replaces drafts; a failed reload retains them", async t => {
  let fail = false; const f = fixture(t, async () => { if (fail) throw Error("offline"); return documentValue(); });
  await tick(); input(f, "Rule ID", "draft"); fail = true; await f.ui.load();
  assert.equal(f.doc.querySelector('label input[type="text"]').value, "draft");
  fail = false; await f.ui.load(); assert.equal(f.doc.querySelector('label input[type="text"]').value, "keep");
});
test("maintenance never starts for another origin and stop aborts pending requests", async t => {
  for (const origin of ["null", "file://", "http://localhost", "https://wails.localhost", "http://wails.localhost:80", "https://example.com"]) {
    assert.throws(() => mountSetup({} as Document, () => { throw Error("must not fetch"); }, origin), /Untrusted/);
  }
  let signal; const f = fixture(t, async () => documentValue()); await tick();
  const dom = new JSDOM(html, { url: "http://wails.localhost" });
  const ui = mountSetup(dom.window.document, async (_, init) => { signal = init.signal; throw Error("offline"); }, "http://wails.localhost");
  ui.stop(); assert.equal(signal.aborted, true); dom.window.close();
});
test("the default export creates one local JSON download despite the document navigation guard", async t => {
  const dom = new JSDOM(html, { url: "http://wails.localhost/" }), doc = dom.window.document;
  const blobs = [], revoked = [], timers = []; let download;
  dom.window.URL.createObjectURL = blob => { blobs.push(blob); return "blob:http://wails.localhost/export"; };
  dom.window.URL.revokeObjectURL = url => revoked.push(url);
  dom.window.setTimeout = callback => { timers.push(callback); return 1; };
  doc.addEventListener("click", event => { if (event.target.closest("a")) event.preventDefault(); });
  dom.window.HTMLAnchorElement.prototype.click = function () {
    const event = new dom.window.MouseEvent("click", { bubbles: true, cancelable: true }); this.dispatchEvent(event);
    download = { name: this.download, href: this.href, blocked: event.defaultPrevented };
  };
  const ui = mountSetup(doc, async (_, init) => new Response(JSON.stringify(JSON.parse(init.body).args.methodName.endsWith("SetupRead") ? documentValue() : { schema_version: 1, records: [] })), "http://wails.localhost");
  t.after(() => { ui.stop(); dom.window.close(); }); await tick();
  doc.querySelector('[data-view="history"]').click(); await tick();
  [...doc.querySelectorAll("#history-panel button")].find(button => button.textContent === "Export history").click(); await tick();
  assert.deepEqual(download, { name: "snoozap-history.json", href: "blob:http://wails.localhost/export", blocked: false });
  assert.equal(blobs.length, 1); assert.equal(blobs[0].type, "application/json"); assert.equal(doc.querySelector("a"), null);
  timers.forEach(callback => callback()); assert.deepEqual(revoked, ["blob:http://wails.localhost/export"]);
});
