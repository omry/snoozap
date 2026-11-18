export type Expression = { probe?: string; all?: Expression[]; any?: Expression[]; not?: Expression };
export type Probe = {
  type: string; enabled: boolean; on_startup_unknown: string; on_error: string;
  grace_seconds: number; interval_ms: number; timeout_ms: number;
  name?: string; executable?: string; wsl_distribution?: string; local_environment?: string;
  source?: string; url?: string; pointer?: string; operator?: string; value?: string;
  credential_headers?: Record<string, string>;
};
export type Draft = {
  schema_version: number; probes: Record<string, Probe>;
  rules: { id: string; enabled: boolean; when: Expression }[];
  settings: { monitoring: boolean; battery: { ac_only: boolean; floor_percent: number; resume_percent: number } };
};

export function node<K extends keyof HTMLElementTagNameMap>(doc: Document, tag: K, text = "") {
  const value = doc.createElement(tag); value.textContent = text; return value;
}
export function button(doc: Document, text: string, action: () => void) {
  const value = node(doc, "button", text); value.type = "button";
  value.addEventListener("click", action); return value;
}
export function field(doc: Document, parent: HTMLElement, label: string, value: string | number | boolean,
  change: (value: string) => void, choices?: string[]) {
  const row = node(doc, "label", label);
  const control = choices ? node(doc, "select") : node(doc, "input");
  if (control instanceof doc.defaultView!.HTMLInputElement) {
    control.type = typeof value === "boolean" ? "checkbox" : typeof value === "number" ? "number" : "text";
    control.autocomplete = "off";
    if (typeof value === "boolean") control.checked = value;
    else control.value = String(value);
    control.addEventListener("input", () => change(control.type === "checkbox" ? String(control.checked) : control.value));
  } else {
    for (const choice of choices!) {
      const option = node(doc, "option", choice); option.value = choice; control.append(option);
    }
    control.value = String(value);
    control.addEventListener("change", () => change(control.value));
  }
  row.append(control); parent.append(row); return control;
}

export function renderRules(doc: Document, root: HTMLElement, draft: Draft, changed: () => void, message: (text: string) => void) {
  root.replaceChildren(node(doc, "h2", "Rules"), node(doc, "p", "Edit conditions, then combine them with ALL, ANY or NOT. Changes take effect only after Save all changes. Native application selection and T3 sign-in will be available in a later delivery slice."));
  const redraw = () => renderRules(doc, root, draft, changed, message);
  const freshExpression = (): Expression => ({ probe: Object.keys(draft.probes)[0] ?? "" });
  const editExpression = (parent: HTMLElement, expression: Expression, replace: (value: Expression) => void, depth = 0) => {
    const box = node(doc, "fieldset"); box.className = "expression"; parent.append(box);
    box.append(node(doc, "legend", "Condition"));
    const kind = expression.all ? "ALL" : expression.any ? "ANY" : expression.not ? "NOT" : "Probe";
    const draw = (value: Expression) => { box.remove(); editExpression(parent, value, replace, depth); };
    field(doc, box, "Match", kind, value => {
      const next: Expression = value === "ALL" ? { all: [freshExpression()] } : value === "ANY" ? { any: [freshExpression()] } : value === "NOT" ? { not: freshExpression() } : freshExpression();
      replace(next); changed(); draw(next);
    }, depth >= 32 ? ["Probe"] : ["Probe", "ALL", "ANY", "NOT"]);
    if (kind === "Probe") {
      const options = [...new Set([expression.probe ?? "", ...Object.keys(draft.probes)])];
      field(doc, box, "Probe", expression.probe ?? "", value => { expression.probe = value; changed(); }, options);
    } else if (expression.not) {
      editExpression(box, expression.not, value => { expression.not = value; }, depth + 1);
    } else {
      const children = expression.all ?? expression.any!;
      const list = node(doc, "div"); box.append(list);
      const renderChildren = () => {
        list.replaceChildren();
        children.forEach((child, i) => {
          const row = node(doc, "div"); list.append(row);
          editExpression(row, child, value => { children[i] = value; }, depth + 1);
          row.append(button(doc, "Remove condition", () => { children.splice(i, 1); changed(); renderChildren(); }));
        });
      };
      renderChildren();
      box.append(button(doc, "Add condition", () => { children.push(freshExpression()); changed(); renderChildren(); }));
    }
  };
  for (const rule of draft.rules) {
    const box = node(doc, "fieldset"); root.append(box); box.append(node(doc, "legend", `Rule: ${rule.id}`));
    field(doc, box, "Rule ID", rule.id, value => { rule.id = value; changed(); });
    field(doc, box, "Rule enabled", rule.enabled, value => { rule.enabled = value === "true"; changed(); });
    editExpression(box, rule.when, value => { rule.when = value; });
    box.append(button(doc, "Delete rule", () => { draft.rules.splice(draft.rules.indexOf(rule), 1); changed(); redraw(); }));
  }
  root.append(button(doc, "Add rule", () => {
    let i = 1; while (draft.rules.some(rule => rule.id === `rule_${i}`)) i++;
    draft.rules.push({ id: `rule_${i}`, enabled: true, when: freshExpression() }); changed(); redraw();
  }));
  root.append(node(doc, "h2", "Probe conditions"));
  for (const [id, probe] of Object.entries(draft.probes)) {
    const box = node(doc, "fieldset"); root.append(box); box.append(node(doc, "legend", `${id} · ${probe.type}`));
    const edit = (label: string, key: keyof Probe, choices?: string[]) => field(doc, box, label, probe[key] as string | number | boolean ?? "", value => {
      (probe as unknown as Record<string, unknown>)[key] = typeof probe[key] === "boolean" ? value === "true" : ["interval_ms", "timeout_ms", "grace_seconds"].includes(key) ? Number(value) : value;
      changed();
    }, choices);
    edit("Probe enabled", "enabled");
    edit("Startup unknown", "on_startup_unknown", ["wait", "hold"]);
    edit("After an error", "on_error", ["retain", "release_after"]);
    edit("Error grace (seconds)", "grace_seconds"); edit("Poll interval (milliseconds)", "interval_ms"); edit("Timeout (milliseconds)", "timeout_ms");
    box.append(node(doc, "p", "Startup hold can prevent sleep before the first successful check. Retain keeps the last confirmed result after errors; release_after drops its hold when the grace period expires."));
    switch (probe.type) {
      case "process":
        // Selector kind is retained. Native/WSL application discovery is 8.2.
        edit(probe.executable ? "Exact executable path" : "Process name", probe.executable ? "executable" : "name");
        edit("WSL distribution (empty for Windows)", "wsl_distribution"); break;
      case "t3": edit("Existing local T3 environment", "local_environment"); break;
      case "power": edit("Power source", "source", ["ac", "battery"]); break;
      case "http":
        edit("Local HTTP URL", "url"); edit("JSON pointer", "pointer");
        edit("Comparison", "operator", ["equals", "not_equals", "exists", "greater_than", "greater_or_equal", "less_than", "less_or_equal"]);
        edit("Expected JSON scalar (for example true, 42 or \"ready\")", "value");
        box.append(node(doc, "p", "Only local HTTP endpoints are permitted. Existing credential references are preserved; this editor does not display or accept secrets.")); break;
    }
    box.append(button(doc, "Delete probe", () => { delete draft.probes[id]; changed(); redraw(); }));
  }
  const add = node(doc, "fieldset"); add.append(node(doc, "legend", "Add local condition")); root.append(add);
  let id = "", type = "http";
  field(doc, add, "New probe ID", "", value => { id = value; });
  field(doc, add, "Condition type", type, value => { type = value; }, ["http", "power"]);
  add.append(button(doc, "Add probe", () => {
    if (!/^[A-Za-z0-9_-]{1,128}$/.test(id) || Object.hasOwn(draft.probes, id)) { message("Use a unique probe ID with 1–128 letters, digits, underscores or hyphens."); return; }
    const probe: Probe = { type, enabled: true, on_startup_unknown: "wait", on_error: "retain", grace_seconds: 60, interval_ms: 5000, timeout_ms: 2000 };
    if (type === "http") Object.assign(probe, { url: "http://127.0.0.1:8080/", pointer: "", operator: "equals", value: "true" });
    else probe.source = "ac";
    Object.defineProperty(draft.probes, id, { value: probe, enumerable: true, writable: true, configurable: true });
    message(""); changed(); redraw();
  }));
}
