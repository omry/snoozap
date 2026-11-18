import { field, node, type Draft } from "./rules.js";

export function renderSettings(doc: Document, root: HTMLElement, draft: Draft, changed: () => void) {
  root.replaceChildren(node(doc, "h2", "Settings"));
  const settings = draft.settings, battery = settings.battery;
  field(doc, root, "Monitor rules", settings.monitoring, value => { settings.monitoring = value === "true"; changed(); });
  root.append(node(doc, "p", "Monitoring off stops rule checks. Manual Stay awake still works. Allow sleep releases protection but continues observing rules when monitoring is on."));
  field(doc, root, "Prevent sleep only on AC power", battery.ac_only, value => { battery.ac_only = value === "true"; changed(); });
  field(doc, root, "Battery floor (%)", battery.floor_percent, value => { battery.floor_percent = Number(value); changed(); });
  field(doc, root, "Battery resume (%)", battery.resume_percent, value => { battery.resume_percent = Number(value); changed(); });
  root.append(node(doc, "p", "The floor must be above 0 and below resume. Resume must be at most 100. Battery policy can suppress both automatic and manual protection; selected mode and timer remain unchanged."));
}
