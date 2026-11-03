# SnooZap UX design — Windows first

Status: the owner selected this Windows-first UX direction and expects to provide
feedback later. This is the current UX baseline for planning, not an implemented
product or a claim that native behavior has been verified. It elaborates the
[current architecture](../../high-level-design.md). That document's existing
review covers its current bytes; UX selection does not supply whole-architecture
approval. Refresh affected review if integration or later feedback changes the
architecture contract. Mac closed-lid mechanisms remain owner-deferred.

The [interactive prototype](ux-prototype.html) uses synthetic data and local
simulation only. It does not change power settings, connect to T3, authorize
access, send wake packets or persist settings. It illustrates core interactions;
native dialogs, full rule editing and external sign-in are not implemented.

## Product goal

A user should be able to keep the computer awake immediately, or understand
whether selected work is keeping it awake, without learning the rule engine.
The common path must stay short. Setup, troubleshooting and optional remote wake
must not be prerequisites for manual use.

## Tray and quick controls

Keep one native notification-area icon. Its shape/badge and tooltip distinguish
an accepted hold, normal sleep allowed, uncertain monitoring and a request
failure. Color is supplementary. The tooltip names SnooZap, the effective state,
the main reason and any manual time remaining.

Primary click opens or focuses the local window. Secondary click opens the
native quick menu. Native keyboard invocation opens the same menu. Suggested
menu order:

```text
SnooZap · Keeping awake                       [status, not a command]
✓ Auto
  Stay awake
  Stay awake for  >  15 minutes / 1 hour / 4 hours / Custom…
  Allow sleep
──────────────────────────────────────────
  Open SnooZap
  Quit SnooZap
```

Stay awake takes effect as soon as selected. No rule, account or settings dialog
is required. Selecting a duration starts/replaces a timed manual hold. Auto and
Allow sleep cancel the manual timer. No confirmation dialog is needed for these
reversible actions. Configured battery limits and OS restrictions still apply.
The mode check mark reflects the selected mode; the separate status line reflects
what SnooZap can actually request. These must not be confused when battery or OS
restrictions prevent a hold.

Quit releases SnooZap's requests and exits. Closing the main window leaves SnooZap
running in the tray. Explain that once on the first close; do not repeatedly
interrupt the user. SnooZap cannot guarantee the icon is outside Windows' hidden
icon area, so setup may show the ordinary Windows way to pin it without changing
taskbar preferences automatically.

## Main window

Use a compact utility window with Status, Rules, History and Settings. Status is
the default page. Keep these in this order above the detailed probe list:

1. **Effective state:** Keeping awake / Normal sleep allowed / Activity unconfirmed
   / Sleep prevention paused / Sleep prevention failed.
2. **Reason:** Active T3 chats, manual mode with remaining time, a retained hold,
   battery cutoff, or an OS request error. Show all other reasons on expansion.
3. **Immediate actions:** Stay awake, Stay awake for…, Auto, Allow sleep.
4. **Current mode and timer:** show when it will return to Auto. Expiry does not
   imply immediate sleep; an active automatic rule can remain a reason.
5. **Probe summary:** readable name, activity/error, last successful check and a
   contextual action such as Reconnect or Edit.

Use a restrained Windows utility appearance, system typography, readable contrast
and normal focus outlines. Do not fill the window with permanent debug fields.
Avoid a generic global “Off” switch: Allow sleep, monitoring paused and Quit have
different effects and need distinct labels.

## Required states and copy

| State | Primary copy and explanation | Useful action |
| --- | --- | --- |
| Active local work | Keeping awake — T3 has active chats | Allow sleep / inspect reason |
| No active work | Normal sleep allowed — no automatic rule matches | Stay awake |
| Timed manual hold | Keeping awake — manual, 58 minutes remaining; then Auto | Change duration / Auto |
| Previously active source unreadable | Keeping awake — activity unconfirmed; last confirmed work retained | Reconnect / inspect policy |
| Startup unknown, default policy | Activity unconfirmed — no automatic hold yet | Stay awake / connect |
| Battery cutoff | Sleep prevention paused — battery 18%, limit 20%; resumes at 25% or AC | Inspect battery settings |
| OS request failed | Sleep prevention failed — SnooZap cannot currently prevent sleep | Retry / details |
| Monitoring off, manual hold active | Keeping awake — manual; automatic monitoring is paused | Auto / enable monitoring |
| Allow sleep while work remains active | Normal sleep allowed — Allow sleep is selected; T3 still reports work | Auto / Stay awake |

Never show a successful protected state merely because a rule matched or a manual
mode is selected. An accepted OS request can still have a platform limitation;
show that limitation beside the status. Keep the screen-off/locking distinction
in explanatory help, not as another prominent switch.

## First launch and setup

First launch is immediately usable for manual control. An empty automatic section
offers Add T3 and Add application, with Advanced check below them. Do not force
an onboarding wizard or enable an implicit application-presence rule.

Add T3 flow:

1. Choose a detected local T3 environment, including a local WSL distribution
   where relevant. Do not offer remote environments as local activity.
2. Connect and open T3's own sign-in/approval flow, explaining read-only access.
3. Confirm the connection and show a plain-language rule: “Keep awake while T3
   has running or waiting chats.” Transitional busy statuses are covered too.
4. Show the defensive connection-failure policy with a Change option, then add
   the rule. Preserve any existing manual mode or timer.

Add application lets the user select a running application or executable. Show
enough identity to distinguish duplicate names and Windows from WSL processes.
An advanced HTTP check exposes the local URL, JSON field/comparison and stored
credential reference. No user command/script editor is included.

## Rules, history and settings

Rules start as readable sentences, with enabled state and current result. Show
AND/OR/NOT through ALL / ANY / NOT groups in an advanced editor; JSON is an
advanced import/export surface, not normal setup. Validate before saving and
retain the previous active configuration on error.

The Monitoring control belongs on Rules. Its off-state explanation says that
automatic checks/holds stop, while manual Stay awake and wake-host holds can
continue. Do not present monitoring-off as proof the machine is allowed to sleep.

History separates API calls from activity/control events. Show time, operation,
duration and sanitized outcome. Expand a row for diagnostics; omit raw payloads
and credentials. Make the 500-record in-memory limit and restart clearing visible
near Clear / Export rather than in the primary status area.

Settings groups Battery, Startup and Optional wake. Keep remote wake off and
collapsed until requested. A host setup explains that keeping this computer
available creates its own awake reason. Remote access instructions must state
the Tailscale access boundary and preserve existing Serve services.

## Wake page

The separate wake-only browser page lists remembered targets with name, current
reachability and last seen time. Each has an explicit Wake button. After a click,
show Packet sent, then Target reachable or Wake unconfirmed. A sleeping target
stays in the list. Do not call a stale, unseen peer “asleep” when its actual power
state is unknown. A target on another network shows unavailable with an explanation.
No settings, probe history or local control buttons appear on this page.

## TUI

Use the same engine snapshot and terminology as the desktop. Keep effective
status, reason, mode and remaining time at the top, followed by probe rows and
the selected detail/history view. A visible footer exposes `w` Stay awake, `t`
Timed, `a` Auto, `s` Allow sleep and `q` Exit/detach. Hotkeys do not activate
while entering text. Attaching to a running engine and owning a foreground
engine must have different exit copy; never imply detaching stops protection.

## Review checks

Verify manual use without setup; timed expiry with and without active work;
timer replacement; monitor-off versus Allow sleep; unknown/error/battery/OS
states; keyboard-only access; focus order; high contrast; text scaling; and clear
separation between selected mode and effective sleep prevention. Also review
T3 setup/reauthorization, unsafe invalid edits, full history, and remembered wake
targets. This prototype demonstrates UX ideas, not native accessibility or power
behavior verification.

Windows interaction context: [Microsoft notification-area guidance](https://learn.microsoft.com/en-us/windows/win32/uxguide/winenv-notification)
and [taskbar keyboard navigation](https://support.microsoft.com/en-us/accessibility/windows/navigate-and-explore-the-windows-taskbar).
The notification-area design guide is explicitly historical Windows 7 guidance;
use its interaction principles rather than treating its presentation as a current
Windows requirement. The taskbar navigation article covers current Windows
keyboard access. The proposed product layout is SnooZap-specific.
