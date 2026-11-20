import { renderRules, node, button, field, type Draft } from "./rules.js";
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
  const call: (method: Parameters<Call>[0] | "ApplicationSetup", args?: unknown[]) => Promise<unknown> = async (method, args = []) => {
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
  function drawRules() {
    const panel = element("rules-panel"), applications = node(doc, "section"), rules = node(doc, "div");
    panel.replaceChildren(applications, rules);
    renderRules(doc, rules, draft!, changed, text => { message.textContent = text; });
    applications.append(button(doc, "Add application", () => {
      if (busy || stopped) return;
      const box = node(doc, "fieldset"), feedback = node(doc, "p"), options = node(doc, "select");
      box.append(node(doc, "legend", "Add application"));
      box.append(node(doc, "p", "Choose a known current-user executable. Inaccessible processes are not listed. Test observes activity without changing rules or sleep prevention. Save all changes applies the completed draft."));
      box.append(node(doc, "p", "WSL inspection requires a running local distribution. If it stops after preflight, inspection can restart it. SnooZap never automatically terminates a distribution."));
      const sources = node(doc, "select"), label = node(doc, "label", "Application source");
      const native = node(doc, "option", "Windows"); native.value = ""; sources.append(native); label.append(sources); box.append(label);
      const apps = node(doc, "label", "Executable"); apps.append(options); box.append(apps);
      let tested = "", generation = 0, closed = false, working = false;
      let policy = "retain", startup = "wait";
      field(doc, box, "After an error", policy, value => { policy = value; }, ["retain", "release_after"]);
      field(doc, box, "Startup unknown", startup, value => { startup = value; }, ["wait", "hold"]);
      box.append(node(doc, "p", "Retain keeps the last confirmed result after an error. release_after releases it after 60 seconds. Startup hold can prevent sleep before a successful check."));
      const controls: HTMLButtonElement[] = [];
      const display = (text: string) => { feedback.textContent = text; };
      const operate = async (action: "sources" | "list" | "test") => {
        if (working || closed || stopped || busy) return;
        working = busy = true; save.disabled = reload.disabled = true;
        sources.disabled = options.disabled = true; controls.forEach(control => { control.disabled = true; });
        const current = ++generation, distribution = sources.value, executable = options.value;
        try {
          const request = action === "sources" ? { action } : action === "list" ? { action, distribution } : { action, distribution, executable };
          const result = await call("ApplicationSetup", [request]) as { items: string[]; observation?: { State: string } };
          if (closed || stopped || current !== generation) return;
          if (!Array.isArray(result.items) || result.items.length > 256 || result.items.some(item => typeof item !== "string" || item.length > 32768)) throw Error("Invalid inventory");
          if (action === "sources") {
            tested = ""; options.replaceChildren();
            sources.replaceChildren(native);
            for (const name of result.items) { const option = node(doc, "option", `WSL: ${name}`); option.value = name; sources.append(option); }
            display("Choose a source, then Refresh applications.");
          } else if (action === "list") {
            tested = ""; options.replaceChildren();
            for (const path of result.items) { const option = node(doc, "option", path); option.value = path; options.append(option); }
            display(result.items.length ? "Select an executable, then Test application." : "No selectable executables found. Refresh after starting the application.");
          } else {
            const state = result.observation?.State;
            if (state !== "active" && state !== "inactive") throw Error("Activity unconfirmed");
            tested = JSON.stringify([distribution, executable]);
            display(state === "active" ? "Application is running. Test changed no settings or sleep request." : "Application is not running. Test changed no settings or sleep request.");
          }
        } catch {
          if (!closed && !stopped) { tested = ""; display(action === "sources" ? "WSL sources unavailable. Windows selection remains available." : "Selection or activity unconfirmed. Your configuration and draft are unchanged. Refresh before trying again."); }
        } finally {
          working = busy = false;
          if (!stopped) { save.disabled = reload.disabled = false; sources.disabled = options.disabled = false; controls.forEach(control => { control.disabled = false; }); }
        }
      };
      sources.addEventListener("change", () => { tested = ""; options.replaceChildren(); display("Refresh applications for the selected source."); });
      options.addEventListener("change", () => { tested = ""; });
      controls.push(button(doc, "Refresh WSL sources", () => void operate("sources")), button(doc, "Refresh applications", () => void operate("list")),
        button(doc, "Test application", () => { if (options.value) void operate("test"); }), button(doc, "Add application rule to draft", () => {
          if (working || stopped || !options.value || tested !== JSON.stringify([sources.value, options.value])) { display("Confirm the selected executable with Test application first."); return; }
          if (Object.keys(draft!.probes).length >= 256 || draft!.rules.length >= 256) { display("The configuration already contains the maximum number of probes or rules."); return; }
          let i = 1; while (Object.hasOwn(draft!.probes, `application_${i}`) || draft!.rules.some(rule => rule.id === `application_${i}`)) i++;
          const id = `application_${i}`;
          draft!.probes[id] = { type: "process", enabled: true, executable: options.value, ...(sources.value ? { wsl_distribution: sources.value } : {}),
            on_error: policy, on_startup_unknown: startup, grace_seconds: 60, interval_ms: 5000, timeout_ms: 2000 };
          draft!.rules.push({ id, enabled: true, when: { probe: id } }); closed = true; generation++; changed(); drawRules();
        }));
      box.append(...controls, button(doc, "Cancel application selection", () => { closed = true; generation++; drawRules(); }), feedback);
      applications.replaceChildren(box);
    }));
  }
  async function load() {
    if (busy || stopped) return;
    busy = true; reload.disabled = true; fields.disabled = true; save.disabled = true;
    try {
      const document = await call("SetupRead") as Parameters<typeof readDraft>[0];
      const next = readDraft(document);
      if (stopped) return;
      draft = next;
      configurationRevision = document.configuration_revision;
      drawRules();
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
