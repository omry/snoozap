import { button, node } from "./rules.js";

type Record = { sequence: number; time: string; probe_id?: string; operation: string; duration_ms: number; result: string; status_code?: number };
export type History = { schema_version: number; records: Record[] };
export type Call = (method: "SetupRead" | "SetupApply" | "HistoryRead" | "HistoryClear", args?: string[]) => Promise<unknown>;

// Export only the typed diagnostics returned by Service. Never include a config
// draft, endpoint, credential reference, raw error, response header or body.
export function safeHistory(value: History): History {
  if (value.schema_version !== 1 || !Array.isArray(value.records) || value.records.length > 1000) throw Error("Invalid history");
  return { schema_version: 1, records: value.records.map(row => ({ sequence: row.sequence, time: row.time,
    probe_id: row.probe_id, operation: row.operation, duration_ms: row.duration_ms, result: row.result, status_code: row.status_code })) };
}

export function mountHistory(doc: Document, root: HTMLElement, call: Call, download: (data: string) => void) {
  root.replaceChildren(node(doc, "h2", "History"), node(doc, "p", "At most 500 probe calls and 500 events. URLs, credentials, headers, response bodies and raw errors are not recorded."));
  const message = node(doc, "p"); message.setAttribute("role", "status");
  const table = node(doc, "table"), head = node(doc, "thead"), headings = node(doc, "tr"), body = node(doc, "tbody");
  for (const label of ["Time", "Probe", "Operation", "Result", "Duration (ms)", "HTTP status"]) { const cell = node(doc, "th", label); cell.scope = "col"; headings.append(cell); }
  head.append(headings); table.append(head, body);
  let busy = false, stopped = false;
  const run = async (method: "HistoryRead" | "HistoryClear", exportFile = false) => {
    if (busy || stopped) return;
    busy = true; controls.forEach(control => { control.disabled = true; }); message.textContent = "";
    try {
      const value = safeHistory(await call(method) as History);
      if (stopped) return;
      if (exportFile) download(JSON.stringify(value, null, 2) + "\n");
      else {
        body.replaceChildren(...value.records.map(record => {
          const row = node(doc, "tr");
          for (const text of [record.time, record.probe_id ?? "", record.operation, record.result, String(record.duration_ms), record.status_code ? String(record.status_code) : ""]) row.append(node(doc, "td", text));
          return row;
        }));
      }
      message.textContent = exportFile ? "Sanitized history exported." : `${value.records.length} records${method === "HistoryClear" ? " after clear" : ""}.`;
    } catch { if (!stopped) message.textContent = method === "HistoryClear" ? "Clear not confirmed. Refresh history before trying again." : "History unavailable. Try Refresh history."; }
    finally { busy = false; if (!stopped) controls.forEach(control => { control.disabled = false; }); }
  };
  const controls = [button(doc, "Refresh history", () => void run("HistoryRead")), button(doc, "Clear history", () => void run("HistoryClear")), button(doc, "Export history", () => void run("HistoryRead", true))];
  root.append(...controls, message, table);
  return { refresh: () => run("HistoryRead"), stop: () => { stopped = true; } };
}
