import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { mountHistory, safeHistory } from "./dist/history.js";

const value = () => ({ schema_version: 1, records: [{ sequence: 1, time: "2026-11-13T00:00:00Z", probe_id: "local", operation: "probe", duration_ms: 4, result: "active", status_code: 200 }] });
test("history export is bounded and strips every untyped field", () => {
  const data = value(); Object.assign(data, { configuration: "secret" }); Object.assign(data.records[0], { url: "http://private/", headers: "secret", body: "private", error: "secret" });
  assert.deepEqual(safeHistory(data), value());
  assert.throws(() => safeHistory({ schema_version: 2, records: [] }));
  assert.throws(() => safeHistory({ schema_version: 1, records: Array(1001).fill(data.records[0]) }));
  assert.equal(safeHistory({ schema_version: 1, records: Array(1000).fill(data.records[0]) }).records.length, 1000);
});
test("read/export are observational, clear is explicit once and text rendering is safe", async t => {
  const dom = new JSDOM('<section id="root"></section>'), root = dom.window.document.getElementById("root"), calls = [], exports = [];
  t.after(() => dom.window.close());
  let data = value(); data.records[0].probe_id = '<img src=x onerror="bad()">';
  const ui = mountHistory(dom.window.document, root, async method => { calls.push(method); if (method === "HistoryClear") data = { schema_version: 1, records: [] }; return data; }, data => exports.push(data));
  await ui.refresh(); assert.equal(root.querySelector("img"), null); assert.match(root.querySelector("tbody").textContent, /<img/);
  const click = text => [...root.querySelectorAll("button")].find(button => button.textContent === text).click();
  click("Export history"); await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(calls, ["HistoryRead", "HistoryRead"]); assert.equal(JSON.parse(exports[0]).records.length, 1);
  click("Clear history"); click("Clear history"); await new Promise(resolve => setImmediate(resolve));
  assert.equal(calls.filter(method => method === "HistoryClear").length, 1); assert.equal(root.querySelector("tbody").children.length, 0);
  ui.stop(); await ui.refresh(); assert.equal(calls.length, 3);
});
test("an uncertain clear is never replayed and retains the displayed records", async t => {
  const dom = new JSDOM('<section id="root"></section>'), root = dom.window.document.getElementById("root"); t.after(() => dom.window.close());
  let clears = 0; const ui = mountHistory(dom.window.document, root, async method => { if (method === "HistoryClear") { clears++; throw Error("secret"); } return value(); }, () => {});
  await ui.refresh(); [...root.querySelectorAll("button")].find(button => button.textContent === "Clear history").click();
  await new Promise(resolve => setImmediate(resolve)); assert.equal(clears, 1); assert.equal(root.querySelector("tbody").children.length, 1);
  assert.match(root.querySelector('[role="status"]').textContent, /not confirmed/); assert.doesNotMatch(root.textContent, /secret/); ui.stop();
});
