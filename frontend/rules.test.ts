import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { renderRules } from "./dist/rules.js";

function fixture(t) {
  const dom = new JSDOM('<section id="rules"></section>'); t.after(() => dom.window.close());
  const doc = dom.window.document, root = doc.getElementById("rules"), draft = { schema_version: 1, probes: {
    proc: { type: "process", name: "Editor.exe", enabled: false, on_startup_unknown: "hold", on_error: "release_after", grace_seconds: 30, interval_ms: 1000, timeout_ms: 200 },
    t3: { type: "t3", local_environment: "local-existing", enabled: true, on_startup_unknown: "wait", on_error: "retain", grace_seconds: 60, interval_ms: 5000, timeout_ms: 2000 },
  }, rules: [{ id: "nested", enabled: false, when: { all: [{ probe: "proc" }, { not: { any: [{ probe: "t3" }] } }] } }], settings: { monitoring: true, battery: { ac_only: false, floor_percent: 20, resume_percent: 25 } } };
  let changes = 0, message = ""; renderRules(doc, root, draft, () => changes++, value => { message = value; });
  const click = text => { const value = [...root.querySelectorAll("button")].find(node => node.textContent === text); assert.ok(value, text); value.click(); };
  const edit = (text, value, index = 0) => {
    const labels = [...root.querySelectorAll("label")].filter(node => node.firstChild?.textContent === text), control = labels[index].querySelector("input,select");
    if (control.type === "checkbox") control.checked = value; else control.value = value;
    control.dispatchEvent(new dom.window.Event(control.tagName === "SELECT" ? "change" : "input"));
    return control;
  };
  return { doc, root, draft, edit, click, get changes() { return changes; }, get message() { return message; } };
}
test("structured editor preserves exact nested ALL/ANY/NOT and existing disabled policies", t => {
  const f = fixture(t), before = JSON.stringify(f.draft); assert.equal(JSON.stringify(f.draft), before);
  assert.deepEqual([...f.root.querySelectorAll("select")].filter(n => n.closest(".expression") && n.parentElement.firstChild.textContent === "Match").map(n => n.value), ["ALL", "Probe", "NOT", "ANY", "Probe"]);
  f.edit("Rule ID", "changed"); f.edit("Rule enabled", true); f.edit("After an error", "retain"); f.edit("Startup unknown", "wait");
  f.edit("Process name", "Other.exe"); f.edit("Existing local T3 environment", "another");
  assert.equal(f.draft.rules[0].enabled, true); assert.equal(f.draft.probes.proc.on_error, "retain");
  assert.equal(f.draft.probes.proc.on_startup_unknown, "wait"); assert.equal(f.draft.probes.proc.name, "Other.exe");
  assert.deepEqual(f.draft.rules[0].when, { all: [{ probe: "proc" }, { not: { any: [{ probe: "t3" }] } }] });
  assert.ok(f.changes >= 6);
});
test("expression operators and child edits change only the selected subtree", t => {
  const f = fixture(t); f.edit("Match", "ANY", 1); f.edit("Probe", "t3", 0);
  assert.deepEqual(f.draft.rules[0].when, { all: [{ any: [{ probe: "t3" }] }, { not: { any: [{ probe: "t3" }] } }] });
  f.click("Add condition"); assert.ok(JSON.stringify(f.draft.rules[0].when).includes('"probe"'));
  f.click("Remove condition"); assert.ok(f.changes >= 4);
});
test("adding HTTP/power probes validates IDs, supports prototype-like IDs and does not add an implicit rule", t => {
  const f = fixture(t); f.edit("New probe ID", "bad id"); f.click("Add probe"); assert.match(f.message, /unique probe ID/);
  f.edit("New probe ID", "__proto__"); f.click("Add probe"); assert.ok(Object.hasOwn(f.draft.probes, "__proto__"));
  assert.equal(f.draft.probes.__proto__.value, "true"); assert.equal(f.draft.rules.length, 1);
  f.edit("New probe ID", "battery"); f.edit("Condition type", "power"); f.click("Add probe");
  assert.equal(f.draft.probes.battery.type, "power"); assert.equal(f.draft.probes.battery.source, "ac");
  f.edit("Power source", "battery"); assert.equal(f.draft.probes.battery.source, "battery");
  f.click("Add rule"); assert.equal(f.draft.rules.length, 2); f.click("Delete rule"); assert.equal(f.draft.rules.length, 1);
});
test("HTTP fields are text, preserve credential references and never interpret markup", t => {
  const f = fixture(t); f.edit("New probe ID", "http"); f.click("Add probe");
  f.draft.probes.http.credential_headers = { Authorization: "opaque-ref" };
  f.edit("Local HTTP URL", "http://localhost:1234/path"); f.edit("JSON pointer", '/<img src=x>'); f.edit("Comparison", "greater_than");
  f.edit("Expected JSON scalar (for example true, 42 or \"ready\")", "123456789012345678901234567890");
  assert.equal(f.draft.probes.http.value, "123456789012345678901234567890");
  assert.deepEqual(f.draft.probes.http.credential_headers, { Authorization: "opaque-ref" });
  assert.equal(f.root.querySelector("img"), null); assert.equal(f.root.querySelector("textarea"), null);
  assert.equal(f.root.querySelector('[type="password"]'), null);
});
test("probe timings, enabled flags and deletion are explicit draft edits", t => {
  const f = fixture(t); f.edit("Probe enabled", true); f.edit("Poll interval (milliseconds)", "3000");
  f.edit("Timeout (milliseconds)", "1500"); f.edit("Error grace (seconds)", "90"); f.edit("WSL distribution (empty for Windows)", "Ubuntu");
  assert.equal(f.draft.probes.proc.enabled, true); assert.equal(f.draft.probes.proc.interval_ms, 3000);
  assert.equal(f.draft.probes.proc.timeout_ms, 1500); assert.equal(f.draft.probes.proc.grace_seconds, 90);
  assert.equal(f.draft.probes.proc.wsl_distribution, "Ubuntu"); f.click("Delete probe");
  assert.equal(Object.hasOwn(f.draft.probes, "proc"), false);
  // Keep the reference for server validation; do not silently weaken a rule.
  assert.equal(f.draft.rules[0].when.all[0].probe, "proc");
  f.edit("Match", "NOT", 0); assert.ok(f.draft.rules[0].when.not);
  f.edit("Match", "Probe", 0); assert.deepEqual(f.draft.rules[0].when, { probe: "t3" });
});
