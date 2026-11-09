import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";
import { mount } from "./dist/status.js";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
function view(sequence = 1, mode = "auto", extra = {}, activity = "unknown") {
  return { State: { Connected: true, Pending: false, Snapshot: { sequence, mode, activity } }, Presentation: {
    Title: "Normal sleep allowed", Reason: "No work observed", Selected: `Selected: ${mode}`, Request: "No accepted request", Icon: "allowed", Limitations: ["Sleep and lid policies still apply"], ...extra,
  } };
}
const response = (value = view()) => new Response(JSON.stringify(value));
const tick = () => new Promise(resolve => setImmediate(resolve));
function fixture(t, fetcher) {
  const dom = new JSDOM(html, { url: "http://wails.localhost/", pretendToBeVisual: true });
  const ui = mount(dom.window.document, fetcher, dom.window.location.origin);
  t.after(() => { ui.stop(); dom.window.close(); });
  const doc = dom.window.document;
  return { dom, doc, ui, input: doc.getElementById("duration"), error: doc.getElementById("error") };
}
test("manual hold keeps observed activity distinct from protection status", async t => {
  let current = view(1, "awake", {
    Title: "Keeping awake", Reason: "Manual Stay awake", Selected: "Selected: Stay awake",
    Request: "Windows accepted the request", Icon: "accepted",
  }, "active");
  const { doc, ui } = fixture(t, async () => response(current));
  await tick();
  const displayed = new Map<string, string>();
  for (const activity of ["active", "inactive", "unknown"]) {
    current = view(current.State.Snapshot.sequence + 1, "awake", current.Presentation, activity);
    await ui.refresh();
    displayed.set(activity, doc.getElementById("activity")!.textContent!);
  }
  assert.deepEqual([...displayed.keys()], ["active", "inactive", "unknown"]);
  assert.deepEqual([...displayed.values()], [
    "Observed activity: Active",
    "Observed activity: Inactive",
    "Observed activity: Unknown",
  ]);
  assert.equal(doc.getElementById("selected")!.textContent, "Selected: Stay awake");
  assert.equal(doc.getElementById("request")!.textContent, "Windows accepted the request");
});
test("allow sleep, auto and disconnect keep activity detection truthful", async t => {
  let connected = true;
  let current = view(1, "allow-sleep", {
    Title: "Normal sleep allowed", Reason: "No work observed", Selected: "Selected: Allow sleep",
    Request: "No accepted request", Icon: "allowed",
  }, "active");
  const { doc, ui } = fixture(t, async () => {
    if (!connected) throw new Error("owner disconnected");
    return response(current);
  });
  await tick();
  assert.equal(doc.getElementById("activity")!.textContent, "Observed activity: Active");
  assert.equal(doc.getElementById("selected")!.textContent, "Selected: Allow sleep");
  current = view(2, "auto", {
    Title: "Normal sleep allowed", Reason: "No work observed", Selected: "Selected: Auto",
    Request: "No accepted request", Icon: "allowed",
  }, "inactive");
  await ui.refresh();
  assert.equal(doc.getElementById("activity")!.textContent, "Observed activity: Inactive");
  assert.equal(doc.getElementById("selected")!.textContent, "Selected: Auto");
  assert.equal(doc.querySelector('[data-mode="auto"]')!.getAttribute("aria-pressed"), "true");
  connected = false;
  await ui.refresh();
  assert.equal(doc.getElementById("activity")!.textContent, "Observed activity: Unknown");
  assert.equal(doc.getElementById("title")!.textContent, "Owner unavailable");
  assert.equal(doc.getElementById("request")!.textContent, "No current sleep-prevention confirmation");
  assert.equal(doc.getElementById("selected")!.textContent, "Mode unavailable");
  for (const button of doc.querySelectorAll('button[data-mode]:not([data-duration])')) {
    assert.equal(button.getAttribute("aria-pressed"), "false");
  }
});
test("only the exact bundled origin starts the interface", () => {
  for (const origin of ["null", "file://", "http://localhost", "http://wails.localhost:1234", "https://wails.localhost", "https://example.com"]) {
    assert.throws(() => mount({} as Document, () => { throw Error("must not fetch"); }, origin), /Untrusted/);
  }
});
test("private reads use the fixed POST binding and have no mutation side effect", async t => {
  const calls = [];
  const { doc } = fixture(t, async (url, init) => { calls.push({ url, init }); return response(); });
  await tick();
  assert.equal(doc.getElementById("title").textContent, "Normal sleep allowed");
  assert.equal(doc.querySelector('[data-mode="auto"]').getAttribute("aria-pressed"), "true");
  const { url, init } = calls[0];
  assert.equal(url, "/wails/runtime"); assert.equal(init.method, "POST");
  assert.equal(init.headers["Content-Type"], "application/json");
  assert.equal(init.redirect, "error"); assert.equal(init.credentials, "omit");
  assert.match(JSON.parse(init.body).args.methodName, /StatusBridge.Read$/);
  assert.equal(calls.length, 1);
});
test("snapshots preserve the custom draft, selection and keyboard focus; labels are text", async t => {
  let current = view();
  const { doc, ui, input } = fixture(t, async () => response(current));
  await tick(); input.value = "1h30m"; input.focus(); input.setSelectionRange(2, 4);
  current = view(2, "awake", { Reason: '<img src=x onerror="alert(1)">', Limitations: ["<script>bad()</script>"] });
  await ui.refresh();
  assert.equal(input.value, "1h30m"); assert.equal(doc.activeElement, input);
  assert.deepEqual([input.selectionStart, input.selectionEnd], [2, 4]);
  assert.equal(doc.querySelector("#reason img"), null);
  assert.equal(doc.querySelector("#limitations script"), null);
  assert.match(doc.getElementById("reason").textContent, /<img/);
});
test("timer ticks stay readable without live announcements; protection changes are announced", async t => {
  let current = view(1, "awake", { Title: "Keeping awake", Reason: "Manual timer · 1m 00s remaining", Selected: "Selected: Stay awake · 1m 00s remaining; then Auto", Request: "Windows accepted the request" });
  let failed = false;
  const { doc, dom, ui, input } = fixture(t, async () => {
    if (failed) throw new Error("owner disconnected");
    return response(current);
  });
  await tick(); input.value = "30m"; input.focus();
  const announcements: string[] = [];
  const observers = [...doc.querySelectorAll('[aria-live]:not([aria-live="off"]), [role="alert"], [role="status"]')].map(region => {
    const observer = new dom.window.MutationObserver(() => announcements.push(region.textContent));
    observer.observe(region, { childList: true, characterData: true, subtree: true });
    return observer;
  });
  t.after(() => observers.forEach(observer => observer.disconnect()));
  for (const remaining of ["0m 59s", "0m 58s"]) {
    current = view(1, "awake", { ...current.Presentation, Reason: `Manual timer · ${remaining} remaining`, Selected: `Selected: Stay awake · ${remaining} remaining; then Auto` });
    await ui.refresh(); await tick();
    assert.match(doc.getElementById("reason").textContent, new RegExp(remaining));
    assert.match(doc.getElementById("selected").textContent, new RegExp(remaining));
  }
  assert.deepEqual(announcements, []);
  assert.equal(doc.activeElement, input); assert.equal(input.value, "30m");
  current = view(2);
  await ui.refresh(); await tick();
  assert.ok(announcements.some(value => value.includes("Normal sleep allowed")));
  assert.ok(announcements.some(value => value.includes("No accepted request")));
  announcements.length = 0;
  failed = true; await ui.refresh(); await tick();
  assert.ok(announcements.some(value => value.includes("Owner unavailable")));
});
test("Custom cancel and an empty confirmation never issue a mode command", async t => {
  const methods = [];
  const { doc, dom, input, error } = fixture(t, async (_, init) => { methods.push(JSON.parse(init.body).args.methodName); return response(); });
  await tick(); input.value = "2h"; doc.getElementById("cancel").click();
  assert.equal(input.value, ""); assert.equal(doc.activeElement, input);
  doc.getElementById("custom").dispatchEvent(new dom.window.Event("submit", { cancelable: true }));
  await tick(); assert.equal(methods.length, 1); assert.match(error.textContent, /positive duration/);
});
test("one confirmation sends one typed command and retains the draft", async t => {
  const calls = []; let sequence = 1;
  const { doc, dom, input } = fixture(t, async (_, init) => {
    const call = JSON.parse(init.body); calls.push(call.args);
    if (call.args.methodName.endsWith("Mode")) sequence++;
    return response(view(sequence, sequence > 1 ? "awake" : "auto"));
  });
  await tick(); input.value = "1h30m";
  const form = doc.getElementById("custom");
  form.dispatchEvent(new dom.window.Event("submit", { cancelable: true }));
  form.dispatchEvent(new dom.window.Event("submit", { cancelable: true }));
  await tick(); await tick();
  const mutations = calls.filter(call => call.methodName.endsWith("Mode"));
  assert.equal(mutations.length, 1); assert.deepEqual(mutations[0].args, ["awake", "1h30m"]);
  assert.equal(input.value, "1h30m"); assert.equal(doc.getElementById("modes").disabled, false);
});
test("a failed or uncertain command is reconciled and never automatically retried", async t => {
  let mutations = 0, reads = 0;
  const { doc, error, ui } = fixture(t, async (_, init) => {
    if (JSON.parse(init.body).args.methodName.endsWith("Mode")) { mutations++; throw new Error("lost response"); }
    reads++; return response();
  });
  await tick(); doc.querySelector('[data-mode="awake"]:not([data-duration])').click();
  await tick(); await tick(); await ui.refresh();
  assert.equal(mutations, 1); assert.ok(reads >= 2); assert.match(error.textContent, /not confirmed/);
  assert.equal(doc.querySelector('[data-mode="auto"]').getAttribute("aria-pressed"), "true");
});
test("a rejected custom duration retains the draft and returns keyboard focus", async t => {
  const { doc, dom, input, error } = fixture(t, async (_, init) => {
    return JSON.parse(init.body).args.methodName.endsWith("Mode") ? new Response("invalid", { status: 400 }) : response();
  });
  await tick(); input.value = "invalid";
  doc.getElementById("custom").dispatchEvent(new dom.window.Event("submit", { cancelable: true }));
  await tick(); await tick();
  assert.equal(input.value, "invalid"); assert.equal(doc.activeElement, input); assert.match(error.textContent, /positive duration/);
});
test("read failure removes the accepted protection claim and disables mutation", async t => {
  let fail = false;
  const current = view(1, "awake", { Title: "Keeping awake", Icon: "accepted", Selected: "Selected: Stay awake · 1m 00s remaining; then Auto" });
  const { doc, ui } = fixture(t, async () => { if (fail) throw Error("disconnected"); return response(current); });
  await tick(); assert.equal(doc.body.dataset.status, "accepted");
  assert.match(doc.getElementById("selected").textContent, /1m 00s remaining/);
  assert.equal(doc.querySelector('[data-mode="awake"]:not([data-duration])').getAttribute("aria-pressed"), "true");
  fail = true; await ui.refresh();
  assert.equal(doc.getElementById("title").textContent, "Owner unavailable");
  assert.equal(doc.body.dataset.status, "unknown"); assert.equal(doc.getElementById("modes").disabled, true);
  assert.equal(doc.getElementById("selected").textContent, "Mode unavailable");
  assert.equal(doc.querySelector('[data-mode="awake"]:not([data-duration])').getAttribute("aria-pressed"), "false");
  fail = false; await ui.refresh();
  assert.equal(doc.getElementById("selected").textContent, current.Presentation.Selected);
  assert.equal(doc.querySelector('[data-mode="awake"]:not([data-duration])').getAttribute("aria-pressed"), "true");
  assert.equal(doc.getElementById("modes").disabled, false);
});
test("a lower-sequence response cannot overwrite a newer snapshot", async t => {
  let current = view(8, "awake", { Title: "Keeping awake" });
  const { doc, ui } = fixture(t, async () => response(current));
  await tick(); current = view(7); await ui.refresh();
  assert.equal(doc.getElementById("title").textContent, "Keeping awake");
});
test("hidden and unloaded views stop reads and prevent further mutations", async t => {
  let count = 0;
  const { doc, dom, ui } = fixture(t, async () => { count++; return response(); });
  await tick(); Object.defineProperty(doc, "hidden", { configurable: true, value: true });
  doc.dispatchEvent(new dom.window.Event("visibilitychange")); await ui.refresh();
  doc.querySelector('[data-mode="awake"]').click(); await tick(); assert.equal(count, 1);
  Object.defineProperty(doc, "hidden", { configurable: true, value: false });
  doc.dispatchEvent(new dom.window.Event("visibilitychange")); await tick(); assert.equal(count, 2);
  dom.window.dispatchEvent(new dom.window.Event("pagehide")); await ui.refresh();
  doc.querySelector('[data-mode="awake"]').click(); await tick(); assert.equal(count, 2);
});
test("link, form and dropped URL navigation handlers prevent default actions", async t => {
  const { doc, dom } = fixture(t, async () => response()); await tick();
  const link = doc.createElement("a"); link.href = "https://example.com"; doc.body.append(link);
  for (const [target, type] of [[link, "click"], [doc, "drop"], [doc, "dragover"], [doc.createElement("form"), "submit"]]) {
    if (target.nodeName === "FORM") doc.body.append(target);
    const event = new dom.window.Event(type, { bubbles: true, cancelable: true }); target.dispatchEvent(event);
    assert.equal(event.defaultPrevented, true, type);
  }
});
