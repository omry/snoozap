import { renderRules, type Draft } from "./rules.js";
import { renderSettings } from "./settings.js";
import { mountHistory, type Call } from "./history.js";

export function readDraft(value: { configuration: string; comparisons: Record<string, string>; configuration_revision: string }): Draft {
  const draft = JSON.parse(value.configuration) as Draft;
  if (draft.schema_version !== 1 || !draft.probes || !Array.isArray(draft.rules)) throw Error("Invalid configuration");
  for (const [id, probe] of Object.entries(draft.probes)) {
    if (probe.type === "http") {
      if (!Object.hasOwn(value.comparisons, id)) throw Error("Missing exact comparison");
      probe.value = value.comparisons[id];
    }
  }
  return draft;
}

export function writeDraft(draft: Draft): string {
  // JSON.parse is used only to check scalar syntax/type. Emit the original
  // literal bytes, never the rounded Number result. Other fields use JSON.stringify.
  const probes = Object.entries(draft.probes).map(([id, probe]) => {
    let encoded = JSON.stringify(probe);
    if (probe.type === "http") {
      const raw = probe.value ?? "true", scalar = JSON.parse(raw);
      if (scalar !== null && typeof scalar === "object") throw Error("Expected a JSON scalar");
      encoded = JSON.stringify({ ...probe, value: undefined }).slice(0, -1) + `,"value":${raw}}`;
    }
    return `${JSON.stringify(id)}:${encoded}`;
  });
  const data = JSON.stringify({ ...draft, probes: undefined }).slice(0, -1) + `,"probes":{${probes.join(",")}}}`;
  if (new TextEncoder().encode(data).length > 1048576) throw Error("Configuration exceeds 1 MiB");
  return data;
}

export function mountSetup(doc: Document, fetcher: (url: string, init: RequestInit) => Promise<Response>, trustedOrigin: string,
  download = (data: string) => {
    const win = doc.defaultView!, url = win.URL.createObjectURL(new win.Blob([data], { type: "application/json" }));
    const link = doc.createElement("a"); link.href = url; link.download = "snoozap-history.json";
    // The status document blocks navigation links. This locally-created,
    // fixed-name download must not reach that document navigation handler.
    link.addEventListener("click", event => event.stopPropagation());
    doc.body.append(link); link.click(); link.remove(); win.setTimeout(() => win.URL.revokeObjectURL(url), 1000);
  }) {
  if (trustedOrigin !== "http://wails.localhost") throw Error("Untrusted application origin");
  const element = <T extends HTMLElement>(id: string) => doc.getElementById(id) as T;
  const form = element<HTMLFormElement>("configuration"), fields = element<HTMLFieldSetElement>("configuration-fields");
  const message = element("setup-message"), save = element<HTMLButtonElement>("save-configuration"), reload = element<HTMLButtonElement>("reload-configuration");
	let draft: Draft | undefined, configurationRevision = "", busy = false, stopped = false, id = 0, dirty = false;
  const pending = new Set<AbortController>();
  const call: Call = async (method, args = []) => {
    if (stopped) throw Error("Closed");
    const controller = new AbortController(); pending.add(controller);
    const timeout = setTimeout(() => controller.abort(), 6000);
    try {
      const response = await fetcher("/wails/runtime", { method: "POST", headers: { "Content-Type": "application/json" },
        cache: "no-store", credentials: "omit", redirect: "error", signal: controller.signal,
        body: JSON.stringify({ object: 0, method: 0, args: { "call-id": `setup-${++id}`, methodName: `github.com/omry/snoozap/internal/desktop.StatusBridge.${method}`, args } }),
      });
      if (!response.ok) throw Error("Request not confirmed");
      return await response.json();
    } finally { clearTimeout(timeout); pending.delete(controller); }
  };
  const changed = () => { dirty = true; message.textContent = "Unsaved changes."; };
  async function load() {
    if (busy || stopped) return;
    busy = true; reload.disabled = true; fields.disabled = true; save.disabled = true;
    try {
      const document = await call("SetupRead") as Parameters<typeof readDraft>[0];
      const next = readDraft(document);
      if (stopped) return;
      draft = next;
      configurationRevision = document.configuration_revision;
      renderRules(doc, element("rules-panel"), draft, changed, text => { message.textContent = text; });
      renderSettings(doc, element("settings-panel"), draft, changed);
      dirty = false; message.textContent = "Saved configuration loaded.";
    } catch { if (!stopped) message.textContent = "Configuration unavailable. Drafts are retained. Try Discard drafts and reload when the owner is available."; }
    finally { busy = false; if (!stopped) { reload.disabled = false; fields.disabled = !draft; save.disabled = !draft; } }
  }
  form.addEventListener("submit", event => {
    event.preventDefault();
    if (busy || stopped || !draft || !dirty) return;
    void (async () => {
      busy = true; fields.disabled = true; save.disabled = true; reload.disabled = true;
      try {
        const result = await call("SetupApply", [writeDraft(draft!), configurationRevision]) as {
          State?: { Snapshot?: { configuration_revision?: string } };
        };
        const nextRevision = result.State?.Snapshot?.configuration_revision;
        if (typeof nextRevision !== "string" || nextRevision.length === 0) throw Error("Save revision unavailable");
        configurationRevision = nextRevision;
        if (!stopped) { dirty = false; message.textContent = "Configuration saved. Selected mode and timer are unchanged."; }
      } catch { if (!stopped) message.textContent = "Save not confirmed. Settings changed elsewhere or the candidate was rejected. Your draft is retained. Reload before trying again."; }
      finally { busy = false; if (!stopped) { fields.disabled = false; save.disabled = false; reload.disabled = false; } }
    })();
  });
  reload.addEventListener("click", () => void load());
  const history = mountHistory(doc, element("history-panel"), call, download);
  for (const tab of doc.querySelectorAll<HTMLButtonElement>("[data-view]")) {
    tab.addEventListener("click", () => {
      for (const other of doc.querySelectorAll<HTMLButtonElement>("[data-view]")) other.setAttribute("aria-pressed", String(other === tab));
      for (const panel of doc.querySelectorAll<HTMLElement>("[data-panel]")) panel.hidden = panel.dataset.panel !== tab.dataset.view;
      if (tab.dataset.view === "history") void history.refresh();
    });
  }
  void load();
  return { load, stop: () => { stopped = true; for (const request of pending) request.abort(); history.stop(); } };
}

if (typeof document !== "undefined" && location.origin === "http://wails.localhost") {
  const ui = mountSetup(document, fetch, location.origin);
  window.addEventListener("pagehide", () => ui.stop(), { once: true });
}
