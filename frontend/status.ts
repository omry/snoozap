type Mode = "auto" | "awake" | "allow-sleep";
type Activity = "active" | "inactive" | "unknown";
type View = {
  State: { Connected: boolean; Pending: boolean; Snapshot: { sequence: number; mode: Mode; activity?: Activity } };
  Presentation: { Title: string; Reason: string; Selected: string; Request: string; Icon: string; Limitations: string[] };
};
type Fetcher = (input: string, init: RequestInit) => Promise<Response>;

// The native guard is authoritative. This interface uses only two fixed calls;
// it never puts private state in global events, HTML or an executable string.
export function mount(doc: Document, fetcher: Fetcher, trustedOrigin: string) {
  if (trustedOrigin !== "http://wails.localhost") throw new Error("Untrusted application origin");
  const element = <T extends HTMLElement>(id: string) => {
    const node = doc.getElementById(id);
    if (!node) throw new Error(`Missing status element: ${id}`);
    return node as T;
  };
  const modes = element<HTMLFieldSetElement>("modes");
  const confirm = doc.querySelector<HTMLButtonElement>('#custom button[type="submit"]')!;
  const disable = (value: boolean) => { modes.disabled = value; confirm.disabled = value; };
  const duration = element<HTMLInputElement>("duration");
  const error = element("error");
  let sequence = -1, requestID = 0, appliedID = 0, busy = false, stopped = false;
  let pendingRead: AbortController | undefined;
  let pendingCommand: AbortController | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const text = (id: string, value: string) => {
    const node = element(id);
    if (node.textContent !== value) node.textContent = value;
  };
  const activityText = (activity: Activity | undefined) => {
    switch (activity) {
      case "active": return "Active";
      case "inactive": return "Inactive";
      default: return "Unknown";
    }
  };
  const render = (view: View, id: number) => {
    if (stopped || id < appliedID || view.State.Snapshot.sequence < sequence) return;
    appliedID = id;
    sequence = view.State.Snapshot.sequence;
    const p = view.Presentation;
    text("title", p.Title); text("reason", p.Reason);
    text("activity", `Observed activity: ${activityText(view.State.Connected ? view.State.Snapshot.activity : "unknown")}`);
    text("selected", p.Selected); text("request", p.Request);
    doc.body.dataset.status = p.Icon;
    disable(busy || !view.State.Connected || view.State.Pending);
    for (const button of doc.querySelectorAll<HTMLButtonElement>("button[data-mode]:not([data-duration])")) {
      button.setAttribute("aria-pressed", String(view.State.Connected && button.dataset.mode === view.State.Snapshot.mode));
    }
    const list = element("limitations");
    if (JSON.stringify(p.Limitations) !== JSON.stringify([...list.children].map(node => node.textContent))) {
      list.replaceChildren(...p.Limitations.map(value => {
        const item = doc.createElement("li"); item.textContent = value; return item;
      }));
    }
    // Preserve input, selection and focus while snapshots and timers change.
  };
  async function call(method: "Read" | "Mode", args: string[], signal: AbortSignal) {
    const id = ++requestID;
    const response = await fetcher("/wails/runtime", {
      method: "POST", headers: { "Content-Type": "application/json" },
      credentials: "omit", cache: "no-store", redirect: "error", signal,
      body: JSON.stringify({ object: 0, method: 0, args: {
        "call-id": `status-${id}`, methodName: `github.com/omry/snoozap/internal/desktop.StatusBridge.${method}`, args,
      } }),
    });
    if (!response.ok) throw new Error("Application request was not confirmed");
    render(await response.json() as View, id);
  }
  const unavailable = () => {
    disable(true);
    doc.body.dataset.status = "unknown";
    text("title", "Owner unavailable"); text("reason", "Protection is unconfirmed");
    text("activity", "Observed activity: Unknown");
    text("request", "No current sleep-prevention confirmation");
  };
  async function refresh() {
    if (stopped || doc.hidden || busy || pendingRead) return;
    const controller = new AbortController(); pendingRead = controller;
    try { await call("Read", [], controller.signal); }
    catch { if (!controller.signal.aborted && !stopped) unavailable(); }
    finally { if (pendingRead === controller) pendingRead = undefined; }
  }
  function schedule() {
    clearTimeout(timer);
    if (stopped || doc.hidden) return;
    timer = setTimeout(async () => { await refresh(); schedule(); }, 1000);
  }
  async function change(mode: string, value = "") {
    if (busy || modes.disabled || stopped || doc.hidden) return;
    busy = true; disable(true); error.textContent = "";
    pendingRead?.abort(); pendingRead = undefined;
    const controller = new AbortController(); pendingCommand = controller;
    let failed = false;
    try {
      await call("Mode", [mode, value], controller.signal);
    } catch {
      failed = true;
      if (!stopped) error.textContent = value ? "Duration was not confirmed. Use a positive duration such as 30m or 1h30m. Check current status before trying again." : "Command was not confirmed. Check current status before trying again.";
    } finally {
      busy = false; pendingCommand = undefined;
      // Reconcile once; never repeat a mutation after an uncertain response.
      await refresh();
      if (failed && value && !stopped && !doc.hidden) duration.focus();
    }
  }
  for (const button of doc.querySelectorAll<HTMLButtonElement>("button[data-mode]")) {
    button.addEventListener("click", () => void change(button.dataset.mode!, button.dataset.duration));
  }
  element<HTMLFormElement>("custom").addEventListener("submit", event => {
    event.preventDefault();
    if (duration.value === "") { error.textContent = "Enter a positive duration, for example 30m."; duration.focus(); return; }
    void change("awake", duration.value);
  });
  element("cancel").addEventListener("click", () => { duration.value = ""; error.textContent = ""; duration.focus(); });
  // Convenience restrictions only. Native origin checks still protect every
  // request if a navigation or popup bypasses these document handlers.
  const prevent = (event: Event) => event.preventDefault();
  doc.addEventListener("click", event => {
    if ((event.target as Element).closest("a")) event.preventDefault();
  });
  doc.addEventListener("submit", prevent);
  doc.addEventListener("dragover", prevent); doc.addEventListener("drop", prevent);
  function visibility() {
    clearTimeout(timer);
    if (doc.hidden) { pendingRead?.abort(); return; }
    void refresh(); schedule();
  }
  function stop() {
    stopped = true; clearTimeout(timer); pendingRead?.abort(); pendingCommand?.abort(); disable(true);
    doc.removeEventListener("visibilitychange", visibility);
  }
  doc.addEventListener("visibilitychange", visibility);
  doc.defaultView?.addEventListener("pagehide", stop, { once: true });
  void refresh(); schedule();
  return { refresh, stop };
}

if (typeof window !== "undefined") {
  window.open = () => null;
  mount(window.document, window.fetch.bind(window), window.location.origin);
}
