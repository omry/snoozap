import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { renderSettings } from "./dist/settings.js";

test("monitoring and battery edits retain unrelated configuration and explain manual behavior", t => {
  const dom = new JSDOM('<section id="root"></section>'); t.after(() => dom.window.close());
  const doc = dom.window.document, root = doc.getElementById("root"), draft = { schema_version: 1, probes: {}, rules: [], settings: { monitoring: true, battery: { ac_only: false, floor_percent: 20, resume_percent: 25 } } };
  let changes = 0; const probes = draft.probes, rules = draft.rules; renderSettings(doc, root, draft, () => changes++);
  const controls = root.querySelectorAll("input"); controls[0].checked = false; controls[1].checked = true; controls[2].value = "35"; controls[3].value = "45";
  controls.forEach(control => control.dispatchEvent(new dom.window.Event("input")));
  assert.deepEqual(draft.settings, { monitoring: false, battery: { ac_only: true, floor_percent: 35, resume_percent: 45 } });
  assert.equal(draft.probes, probes); assert.equal(draft.rules, rules); assert.equal(changes, 4);
  assert.match(root.textContent, /Manual Stay awake still works/); assert.match(root.textContent, /Allow sleep releases protection/);
  assert.match(root.textContent, /selected mode and timer remain unchanged/i);
  assert.equal(root.querySelector('[type="submit"]'), null); assert.doesNotMatch(root.textContent, /startup/i);
});
