# SnooZap design overview

Status: current requirements, architecture and detailed-plan reviews are clean.
The architecture and selected UX are owner-approved, including the amended
WSL process-check policy: skip an observed stopped distribution, accept the
rare restart race, and never automatically shut it down. See
[WSL evidence and decision](supporting/implementation-plan/wsl-lifecycle-evidence.md)
and [rename provenance](supporting/rename-notes.md). Original reviews remain
historical evidence; current helper-owned proofs bind the reviewed bytes.
The owner approved the final eighteen-slice plan, implementation and checkpoint
commits. Remote reviews and draft PR-stack publication are selected, including
publishing the initial design baseline to `main`. Product implementation has not
started. Merges and release publication are not authorized.

Keep the local computer awake for selected work, with immediate manual controls
and clear reasons. Build Windows first, then Mac, then Linux. Optional LAN/Tailscale
wake is a separate wake-only role. See [requirements](requirements.md).

The [architecture](high-level-design.md) uses one local Go engine, native power
adapters, a Wails tray/window and Bubble Tea TUI. Local process/T3/HTTP/power probes
share AND/OR/NOT rules and defensive per-probe failure policies. The selected
[UX](supporting/high-level-design/ux-design.md) makes Stay awake and timed controls
usable without setup; status distinguishes selected mode from effective holds.

The [implementation plan](implementation-plan.md) defines internal contracts and
uses [one ordered slice inventory](slices.json). Shared engine and full Windows
work precede Mac investigation/port/closed-lid verification, then Linux. Estimates
count final incremental nonblank physical production/test lines, not edit churn.

Next step: establish the approved scoped baseline checkpoint, verify clean
committed inputs and classification, then start Windows delivery under remote
review with draft PR-stack publication. The baseline will retain the design,
selected support, review sidecars and an applicable classification policy before
product work. Review scratch and external authority ledgers remain outside the
committed product inputs.

Windows is the first actionable delivery after the baseline and admission gates. Full-project
readiness remains partial because the Mac closed-lid mechanism is owner-deferred.
Complete native Windows/WSL acceptance before real-Mac investigation. Obtain the
Mac mechanism/privilege/installation decision and affected review before its
dependent implementation. Complete Mac acceptance before Linux. All eighteen
slices remain in scope; no release publication or hardware acceptance is implied.

<!-- awd:slice-table:start -->
| Status | Slice ID | Short description | Production lines | Test lines |
| --- | --- | --- | ---: | ---: |
| ⏳ Pending | `snoozap/1` | Usable Windows manual console control | 650–950 | 700–1,100 |
| ⏳ Pending | `snoozap/2` | Application rules, battery and timed controls | 800–1,200 | 1,100–1,650 |
| ⏳ Pending | `snoozap/3` | Safe settings, credentials and diagnostics | 650–1,000 | 550–900 |
| ⏳ Pending | `snoozap/4` | One per-user owner and local command interface | 650–1,000 | 650–1,000 |
| ⏳ Pending | `snoozap/5` | T3 OAuth and local Windows/WSL probes | 1,000–1,500 | 1,000–1,600 |
| ⏳ Pending | `snoozap/6` | Typed HTTP checks and probe scheduling | 450–700 | 550–850 |
| ⏳ Pending | `snoozap/7` | Tray and status window usable without setup | 650–1,000 | 500–800 |
| ⏳ Pending | `snoozap/8` | Add T3/apps, edit rules and inspect history | 850–1,350 | 700–1,100 |
| ⏳ Pending | `snoozap/9` | TUI with the same status and manual controls | 600–950 | 500–850 |
| ⏳ Pending | `snoozap/10` | Optional LAN targets and truthful wake outcomes | 850–1,300 | 850–1,300 |
| ⏳ Pending | `snoozap/11` | Wake-only page through existing Tailscale Serve | 700–1,100 | 750–1,150 |
| ⏳ Pending | `snoozap/12` | Windows portable release and integrated acceptance | 300–550 | 700–1,100 |
| ⏳ Pending | `snoozap/13` | Mac hardware investigation and owner decision | 0–0 | 0–0 |
| ⏳ Pending | `snoozap/14` | Mac assertions, local services and menu bar | 700–1,100 | 650–1,050 |
| ⏳ Pending | `snoozap/15` | Conditional Mac closed-lid implementation | 800–1,600 | 900–1,800 |
| ⏳ Pending | `snoozap/16` | Mac app/CLI and physical platform acceptance | 250–500 | 600–1,000 |
| ⏳ Pending | `snoozap/17` | Linux inhibitors, credentials and desktop integration | 700–1,150 | 650–1,050 |
| ⏳ Pending | `snoozap/18` | Linux release and full-project acceptance | 250–500 | 650–1,100 |
| | **Project total** | Current slices; replaced parents excluded. | **10,850–17,450** | **12,000–19,400** |
<!-- awd:slice-table:end -->
