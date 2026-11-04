---
plan_key: snoozap
slice_convention: hierarchical-v1
slice_inventory: {"executable": false, "path": "slices.json", "sha256": "ab6362540cf44f0eea286c81e1a97fbbfad9633216a8d36ffc21454df72004be"}
---
# SnooZap implementation plan

Status: detailed planning draft with owner-approved architecture, selected UX
and the WSL process-check lifecycle amendment. Final slice-plan approval and
product implementation authority have not been obtained. Windows engineering
can be reviewed as a concrete first delivery. The Mac closed-lid mechanism is
explicitly owner-deferred; its dependent implementation and Mac completion are
not ready. Linux follows complete Mac delivery. The whole project is not yet
implementation ready.

Governing documents: [requirements](requirements.md) and
[architecture](high-level-design.md). Pre-rename approval evidence is retained for their original reviewed bytes.
The name-only revisions and subsequent WSL amendment do not inherit old exact-byte
review stamps; see [rename provenance](supporting/rename-notes.md) and the
[WSL evidence and owner decision](supporting/implementation-plan/wsl-lifecycle-evidence.md).
[Selected UX notes](supporting/high-level-design/ux-design.md) elaborate
the interaction design. The [simulation](supporting/high-level-design/ux-prototype.html)
is context, not production code or native verification.

The single canonical ordered delivery inventory is [slices.json](slices.json).
It owns slice titles, complete descriptions, dependencies, paths, acceptance and
line ranges. This plan owns the plan key and engineering specification; it does
not embed a second inventory. [Overview](overview.md) is the short front page.
[Engineering evidence and estimate basis](supporting/implementation-plan/engineering-evidence.md)
records inspected prerequisites and uncertainty.

## Engineering baseline and delivery gates

There is no product source to adapt. Start a Go module `github.com/omry/snoozap`.
Initial exact pins are Go 1.26.10, Wails v3.1.0, Bubble Tea v2.1.1 via
`charm.land/bubbletea/v2`, and official MCP Go SDK v1.8.0. Use standard Go HTTP,
JSON, contexts and tests, plus native platform calls behind build constraints.
Pin other direct native/discovery dependencies at implementation after API/source
inspection and record them in go.mod/go.sum. No floating `latest` in reproducible
builds. Validate the selected versions together before product integration; a
load-bearing incompatibility returns to the affected architecture decision.

The first Windows delivery candidate is Windows 11 x64, standard user, native
T3 or a locally running supported WSL distribution, and Edge WebView2 for desktop.
Verify ARM64 separately before claiming support. This is a verification target,
not a promise that every Windows/NIC/power model works. Record S3 and Modern
Standby AC/DC separately. Console-only builds exclude Wails/webview dependencies.
The full public OS/hardware support matrix is populated from actual acceptance,
not from successful cross-compilation. Mac versions/hardware and Linux desktops
are selected and tested in their phases before support claims.

Windows completes every agreed Windows journey, including optional wake roles,
before Mac investigation begins. Mac completion includes required closed-lid
work under declared conditions before Linux implementation begins. A temporarily
unavailable test machine does not justify marking a platform delivered. Do not
ship an early Linux helper for the media PC. WSL integration is part of Windows.

## Code ownership and interfaces

Use one shared engine and a small set of native adapters. Avoid a service bus,
plugin ABI, scripting runtime or a second daemon. Neither the desktop framework
nor the TUI owns an independent copy of observations or holds.

| Module | Responsibility and dependency direction |
| --- | --- |
| `internal/model`, `internal/config`, `internal/history` | Shared values, strict config, safe persistence and bounded diagnostics; no UI imports |
| `internal/engine` | Single event owner, policy, rules and scheduling; depends on adapter interfaces |
| `internal/probe` | Process/T3/HTTP/power checks; reports observations, never requests sleep prevention |
| `internal/platform/{windows,darwin,linux}` | Native power, battery, current-user processes, credentials and lifecycle |
| `internal/ipc`, `internal/cli` | User-restricted attach/control protocol and approved CLI shapes |
| `internal/desktop`, `frontend` | Wails tray/window and bundled UI; calls the local service |
| `internal/tui` | Bubble Tea views/input; same service or attached IPC client |
| `internal/wake`, `web/wake` | Separate optional discovery/WOL and wake-only listeners/page |
| `cmd/snoozap`, `cmd/snoozap-tray`, `build`, `docs` | Composition, native entry points, reproducible packaging and shipped instructions |

Define internal Go interfaces with these responsibilities. Concrete wire DTOs
remain separate from OS handles and credentials:

```go
type Probe interface {
    Check(context.Context) Observation
}
type PowerBackend interface {
    SetRequired(context.Context, bool) error
    Close() error
}
type CredentialStore interface {
    Read(context.Context, CredentialRef) ([]byte, error)
    Write(context.Context, CredentialRef, []byte) error
    Delete(context.Context, CredentialRef) error
}
type Service interface {
    Snapshot(context.Context) (Snapshot, error)
    Execute(context.Context, Command) (CommandResult, error)
    Subscribe(context.Context) (<-chan Snapshot, error)
}
```

Inject Clock/Timer, process inventory, system-power reader, network selector,
credential store, opener and MCP/HTTP clients. Native OS resources never enter
status JSON. Only the composition layer selects implementations. Tests can drive
state without sleeping or invoking a real OS power request.

## Serialized state and observation lifetime

The engine goroutine owns config generation, monitoring epoch, observations,
rule hold memory, manual mode/deadline, battery guard, host hold intent and the
last native request result. Adapter workers return immutable results tagged with
probe ID, config generation and epoch. Discard obsolete results after edits,
disable/re-enable or shutdown. Snapshot sequence numbers increase monotonically
within one runtime; clients replace snapshots atomically and can reconnect for
a complete current snapshot. A slow subscriber receives the latest snapshot,
not an unbounded event backlog.

Poll every enabled probe immediately on enable, then at its configured interval.
Limit total concurrent checks to eight and at most one per probe. Scheduled work
that is still running is not duplicated. Every poll has a context timeout; shared
T3 connection setup has a separate two-minute interactive deadline and cancel
path. An authorization page can remain open, but it cannot turn into an infinite
poll or retain an OAuth callback listener after cancellation. Use bounded retries
for transient connection setup only; observations do not hide a failed attempt.

An observation contains activity, completed-at, last-success-at, error category,
config generation/epoch and success history. First failed observation starts the
failure episode; continued failures do not extend release-after grace. A completed
successful observation resets it. Scheduling starvation or a check that ignores
cancellation produces unknown rather than leaving an old active result fresh
forever. The engine owns the deadline even if an adapter worker misbehaves; cap
abandoned workers and stop scheduling a wedged adapter until reset.

On resume, invalidate observations and reconcile mode expiry and fresh battery
state before acquiring. Preserve existing rule-hold memory only under approved
retention semantics during refresh. No old observations are restored on process
restart. Editing a probe resets affected observations/rule history, not unrelated
rules or a manual timer. Monitoring off cancels checks and automatic holds;
turning it back on starts a fresh epoch.

## Rule evaluation and hold controller

Evaluate a rule into `(truth, uncertaintyContributors)`. A known result has no
uncertainty contributors. A leaf unknown contributes its probe ID. NOT preserves
unknown contributors. ALL returns false if any child is false; otherwise unknown
with the union of unknown children, otherwise true. ANY returns true if any child
is true; otherwise unknown with the union, otherwise false. This also specifies
mixed nested expressions without an exponential expansion into all possible
worlds. De-duplicate repeated probe references when applying policies.

For each enabled rule retain a separate prior hold bit. True sets it; false
clears it. Unknown with an existing hold retains only while every contributor's
on-error policy permits retention. An expired release-after contributor clears
that rule hold. Unknown without an existing hold can acquire startup protection
only if all contributors have never succeeded in this epoch and all select
startup hold. A probe that previously succeeded cannot regain startup status
merely because it fails again. A different confirmed rule remains sufficient.

Reconcile in this order on every relevant event:

1. Expire a manual deadline and return to Auto if due.
2. Evaluate current observations/rules and update per-rule hold memory.
3. Determine causes: manual awake, or in Auto any rule hold/wake-host hold.
4. Apply Allow sleep and the battery guard to every cause.
5. Call the native backend only when required state changes, or after a known
   resource-loss/resume event requiring reacquisition.
6. Publish desired request, API acceptance, reasons and known platform limits.

Repeated snapshot reads must not increment Windows power request counts. Release
attempts and errors remain observable; close the native handle on shutdown even
when an explicit clear fails. Retry an operational native failure with bounded
backoff (one, two, five, then thirty seconds), cancelled immediately by a release
or shutdown. Retries repair a failed native call; they do not renew a successful
request to bypass Modern Standby policy.

Manual deadlines use an elapsed-time clock that includes suspend, paired with a
wall timestamp for display. On Windows use a suspend-inclusive native clock;
other ports provide its equivalent. Reconcile missed expiry immediately after
resume. Changes to calendar time cannot extend a duration. Timers are in-memory,
replace the previous timer, and reset to Auto after runtime exit. Battery floor
and recovery use fresh telemetry, including confirmed battery absence; errors
on a battery-capable machine block holds. Preserve why a requested manual hold
is suppressed in the snapshot.

## Configuration, storage and credentials

Use the HLD version-1 schema and defaults unchanged. Decode with duplicate-key
checking and strict per-type fields, bounded to 1 MiB config, 256 probes, 256 rules,
32 expression levels and 4096 expression nodes. These are parser/resource bounds,
not a new rule language. Reject invalid refs, ambiguous operators, empty groups,
unsupported schema and incompatible fields before any candidate becomes active.
Compare JSON numbers with exact decimal semantics for scalar ordering/equality;
do not round all input through float64 or coerce strings/booleans into numbers.

Windows uses a current-user local-app-data folder with a restrictive ACL; Unix
ports use user config/data paths, private directories and regular files with
0600 mode. Reject symlink/reparse redirection for protected state and credential
bindings. Separate config, machine-local environment bindings and remembered
wake targets. The runtime is their sole writer. Manual modes, observations,
history and request records are not persisted. No database is required.

Validate the entire candidate, write a same-directory temporary file with private
permissions, flush it and atomically replace the destination using the native
platform operation. Activate the new generation only after persistence succeeds.
A persistence failure leaves the old valid config active. Save the previous file
for explicit schema migrations; none exists yet. Unknown schema does not silently
reset to defaults. Startup with unreadable/invalid config reports the problem,
allows direct manual controls through an empty validated engine configuration,
and does not overwrite the invalid file or create an implicit rule.

Store OAuth/token/header material only in Windows Credential Manager, macOS
Keychain or Linux Secret Service through a narrow adapter. Config references are
opaque local IDs. Tokens are never placed in command arguments, environment dumps,
IPC status, browser assets, exported config or logs. Failure to store credentials
aborts credentialed setup; there is no plaintext fallback. Setup writes a binding
only after identity validation and credential persistence. On cancelled/failed
setup delete temporary secrets. Export carries IDs without endpoint binding or
secret material; import requires local rebinding. Delete removes the associated
binding and credentials without affecting another probe's shared reference.

History records only the HLD's safe fields, using separate 500-entry rings. Build
safe diagnostic values at the adapter boundary instead of logging raw errors and
trying to redact later. URLs stored in history contain no userinfo, query or
fragment; OAuth paths/codes, headers and response payloads never enter records.
Untrusted names are rendered as text, and terminal control sequences are escaped.

## Local ownership, IPC and frontend bridge

Windows singleton identity is the current-user SID, independent of data-dir.
Use a named mutex plus a local-only named pipe protected for that SID; deny remote
pipe clients. The owner creates protected resources before accepting control.
A crashed owner releases kernel resources; no stale lock-file takeover is needed.
Unix ports use a private user runtime directory, lock and socket with peer UID
verification. A second start attaches/focuses or reports the existing owner; it
cannot create another hold. Failure to identify the owner is an error, not a
reason to spawn a replacement.

Use length-prefixed UTF-8 JSON with a maximum 1 MiB frame and an internal protocol
version of 1. Requests contain request ID, version, operation and typed arguments;
responses contain the matching ID and result or bounded error. Allow only the
approved local command set and setup/test operations. Mutating commands have
connection-local de-duplication IDs; reconnecting clients resolve uncertain
results through the current snapshot rather than silently replaying a mutation.
A version mismatch gives an upgrade message and never starts a competing owner.

The desktop owner can call Service directly; when a headless owner already exists
it uses the same restricted IPC. Wails exposes only the fixed local bridge methods,
not arbitrary file reads, shell or network dispatch. Bundle trusted assets; block
navigation/popups/external script loading and open approved sign-in links through
the system browser after validation. Go callbacks/commands cross to Wails' UI
thread using its supported dispatch. Snapshot subscriptions stop with the view.
TUI output/input never blocks the engine or native shutdown.

## Probe-specific engineering

**Windows processes.** Use native process enumeration and token SID filtering,
with exact executable-path or explicit process-name matching. Normalize Windows
path spelling/case through native APIs; never use command-line substring matches.
A process disappearing during lookup is a normal race, but inaccessible matching
candidates or incomplete enumeration makes the result unknown. A complete empty
set is inactive. Keep other-user activity and full process arguments out of the UI.

**WSL processes.** Enumerate distributions and running status through fixed
`wsl.exe` operations. Bind to a particular registered distribution and its default
user's numeric UID. Immediately before each fixed `/usr/bin/id` or `/usr/bin/ps`
execution, obtain a current running-state result for that distribution. If it is
stopped, missing or enumeration fails, launch no inspection command and report
unknown. Use argument arrays and no shell/user template.
The check and execution are not atomic: a distribution can stop between them,
and `wsl.exe` can restart it. This owner-approved race is a declared limitation,
not proof of an absolute no-start guarantee. Do not add a resident WSL helper or
private COM path to avoid it. Never terminate a distribution after inspection;
there is no reliable exclusive-start ownership, and another user action or
application may already depend on it. Do not claim SnooZap caused a restart from
running-state samples alone. Show the best-effort skip/restart limitation during
WSL application selection and in its settings. T3 polling continues through its
existing authenticated local endpoint without process-inspection launches.
For name checks read only `comm`; for path checks read only `exe`, one complete
field per line. Bound output and reject malformed/truncated/unrepresentable
records. Verify procps availability and semantics in the Windows/WSL acceptance
fixture; missing capability is unknown. A stopped or missing distribution is a
visible unavailable source, not proof that all monitored work ended. Verify both
paths: an observed stopped distribution receives no inspection launch; a
controlled stop after a running result may restart it, with no automatic
termination or false no-start claim. The native isolated lifecycle experiment
and exact owner approval are retained in the linked evidence; product acceptance
must still verify the implemented preflight, fixed commands and UI explanation.

**T3.** Use streamable HTTP MCP and supported OAuth discovery/PKCE, callback state
validation and a random loopback callback port. Request/verify read-only access.
Use the endpoint selected by local setup, never connected remote environments.
Validate an environment/server identity through the supported API and local
endpoint ownership, including WSL/native port collision checks; store it with
the binding. If the current T3 interface cannot prove which local environment
answered, stop setup and return the integration choice to HLD. Do not add a
silent database scrape or executable-script fallback.

Query only the thread-list metadata needed for activity. Include all projects,
subagents and busy statuses from the HLD. A valid nonempty busy result is active;
a complete empty result is inactive. Follow pagination to prove emptiness when
client-side filtering is necessary; truncated/incompatible answers are unknown.
Pin the verified T3 tool schema in adapter contract fixtures. Revocation, expiry,
unreachable transport and unknown schema report bounded unknown. Do not auto
request stronger permissions or prompt repeatedly while polling. Reconnect is an
explicit user setup action. A live MCP connection does not itself prove activity.

**HTTP.** Validate every actual TCP destination as loopback using a custom dialer;
disable environment proxies and redirects. Resolve localhost explicitly and
reject nonloopback resolved addresses before sending credentials. Validate header
names/values and forbid overriding routing headers such as Host. Require 2xx,
limit response to 1 MiB, use typed JSON Pointer and exact scalar comparisons.
Missing pointer is inactive for exists and unknown for other operations. Decode
errors, type mismatch and invalid TLS remain unknown; never disable TLS checking.

**Power source.** Reuse the native system-power reader; do not query another
frontend's cached interpretation. Confirmed source is active/inactive per selected
AC/battery condition. Unknown power data is an unknown probe and separately
triggers the battery guard where the machine is battery-capable.

## Desktop and text behavior

Implement the selected UX with native tray quick commands, a compact default
Status view and Rules/History/Settings. Shared view-model derivation owns wording
for activity, desired hold, API acceptance, manual countdown and suppression
reasons. Preserve focus, input drafts and expanded details while new snapshots
arrive. No per-second whole-window replacement. Follow system theme, scaling,
keyboard focus and accessibility semantics. Status changes are live-readable
without announcing every poll.

Add T3/app/HTTP flows submit validated config through Service. External sign-in
belongs to the system browser; no credential entry inside the embedded window.
A Test check is observational, does not enable a rule or acquire a hold and cannot
become a second background scheduler. Setup must preserve current manual mode
and timer. Editing rules uses the exact approved ALL/ANY/NOT grammar. First
launch has no implicit rule and manual control remains visible throughout setup.

Tray primary click opens/focuses the window; secondary/keyboard invocation shows
quick controls. Close hides; Quit stops all owned runtime work. Autostart is
explicit opt-in, current-user only, and cleared when disabled. Do not change
Windows taskbar pin preferences. A foreground TUI owner exits cleanly on q;
an attached TUI detaches without stopping protection. Shortcut letters become
ordinary text inside editing dialogs.

## Wake networking and target lifecycle

Choose one current physical LAN adapter through the approved settings field.
Use native stable adapter identity plus a network association derived from native
network profile/link information and current subnet/gateway evidence. Exclude
loopback, Tailscale/VPN, WSL virtual and tunnel adapters. Changes/ambiguous identity
make old targets unavailable until rediscovered on the selected current network;
do not infer same LAN merely from a common private prefix. Native selection and
identity behavior is a per-platform acceptance prerequisite.

Use DNS-SD `_snoozap._tcp.local.` version-1 advertisements on that interface.
Advertise only bounded display name, persistent random installation ID, protocol
version, selected physical MAC and wake-only LAN service port. Persist the local
installation ID in private data. Validate peer fields and receiving interface,
reject oversized/duplicate/conflicting metadata and display names as text. Do
not execute or follow URLs from advertisements. mDNS is an unpaired, unauthenticated
same-link discovery mechanism; reappearance is observational identity, not
cryptographic authentication.

Remember at most 256 targets and never silently evict sleeping entries. Store
network association, selected adapter/MAC, last-seen and protocol version. A live
TTL-expired peer becomes not_seen, not definitely asleep. Wrong-network targets
remain visible but unavailable. Forget is an explicit local user operation.
Use the connected-network address and interface index to reject off-link LAN
clients; ignore forwarded identity headers there. Bind the LAN listener only to
the selected interface and document any user-approved firewall rule prerequisite.
Do not auto widen the firewall or expose local IPC/config/diagnostics.

The LAN listener also enforces the approved browser CSRF boundary. Its existing
targets GET returns a random, short-lived `X-SnooZap-CSRF` response header, with
`Cache-Control: no-store` and no cross-origin read permission. A LAN wake POST
must echo that token; bind it to the requesting source and selected interface,
expire it after fifteen minutes and bound the token/session table to 256 entries.
If Origin is present, require an exact match with the selected listener's origin;
validate Host against the bound address/port to reject DNS-rebinding aliases.
Missing/expired tokens and cross-site requests fail before sending a packet.
Native LAN clients can GET a token without pairing or an account, then POST it;
this is browser request integrity, not a new peer authentication mechanism.
Test hostile simple POSTs, cross-origin reads, forged Host, expired tokens and
ordinary unpaired native clients. Keep these tokens out of logs and diagnostics.

WOL sends three bounded standard magic packets (six FF bytes plus sixteen MAC
repetitions) to the selected subnet broadcast on UDP port 9; no VPN/unrelated
interface broadcast. A send error is send_failed. An explicit request already
reachable returns already_reachable without another send. Otherwise record
packet_sent and wait at most sixty seconds for a matching installation on the
same selected LAN to reappear; then target_reachable or unconfirmed. No claim
about unlocked login or T3 readiness. Real firmware/NIC/sleep support must pass
before a target combination is documented as supported.

The separate loopback remote backend exposes only the three HLD HTTP operations
and bundled wake page. It trusts Serve user identity only on that dedicated route;
LAN traffic never enters it. Local same-user processes remain within the trusted
local-user boundary and can already control SnooZap through IPC. Remote responses
include no MAC, local probes, configuration or history. Preserve schema-version-1
field semantics and bounded error codes exactly as the HLD.

GET of the page establishes an opaque in-memory session and anti-CSRF token;
POST requires same Origin and token. Configure exact expected HTTPS origin from
the inspected Serve URL/port; ignore arbitrary forwarded Host. Use secure HttpOnly
SameSite cookies on the browser-facing origin, escape text and set restrictive
CSP. Deny absent Serve user identity, cross-origin, unsafe verbs/body, and unknown
target/request IDs. Bound requests to 16 KiB headers, 1 KiB empty/JSON body check,
256 outstanding session records and 500 wake requests, expiring old requests
at fifteen minutes. Rate-limit by authenticated user or LAN source and target
(one target action per five seconds, sixty client actions per minute).

Inspect `tailscale serve status --json` read-only and validate the selected HTTPS
port has no conflicting mapping. Display a user-run command equivalent to
`tailscale serve --bg --https=8443 http://127.0.0.1:<backend-port>` and exact URL/access
guidance. Never execute setup/reset or enable Funnel. Bind backend port and retain
it in machine-local setup; if another process occupies it, remote hosting reports
unavailable until explicit re-setup, rather than stealing the port/mapping. Keep
existing unrelated services intact. Deny clients without user identity, including
tagged-device flows; policy-authorized shared users can wake targets as disclosed.

An uncertain POST is not retried automatically. A retained request disappearing
after restart/expiry returns 404 unavailable, never a claim that nothing was sent.
Host role adds its existing controller cause, still overridden by Allow sleep and
battery. Target advertisement alone does not hold the machine awake. Warn when
host availability is suppressed; a sleeping host cannot receive a request.

## Platform-specific remaining work

Windows power handles balance Set/Clear on transitions. Register resume/power
notifications and use GetSystemPowerStatus plus capability queries to identify
battery absence and Modern Standby limits. Register user autostart without admin.
Release resources on graceful stop; crash/kill kernel-handle cleanup is verified.
Portable desktop and console output have independent native subsystem entry
points but share all policy/config/probe code. Ship WebView2 prerequisite guidance;
console operation must not load desktop libraries.

Mac ordinary idle protection uses IOKit assertions; use native notifications,
Keychain and current-user inventory, then reuse common IPC/UI/TUI/wake behavior.
The first Mac slice investigates actual closed-lid behavior on hardware. Keep
`closed_lid` unavailable until an explicit mechanism decision and affected review.
The future adapter must reconcile desired enabled state, ownership, battery/mode
suppression and restore/recovery; that is an interface responsibility, not an
approved privileged helper implementation. Any system-wide change, installer,
lease or recovery mechanism requires the retained owner decision. No ordinary
assertion-only or external-display substitute completes the required outcome.

Linux uses logind's narrow idle inhibitor with descriptor lifetime plus desktop
policy verification; test that it preserves explicit user Suspend and display-off.
Use UPower/native notifications, current-user procfs, Secret Service and shared
Unix IPC. Verify supported desktop tray/webview dependencies and optional user
session autostart. Missing native services fail visibly. This is future Linux
work after full Mac completion, not an implicitly authorized media-PC helper.

## Validation, coverage and completion

Use Go unit/property tests for rule/controller/config boundaries, bounded local
protocol fixtures for adapters/IPC/wake handlers, and frontend interaction tests
for UI state/focus. Use a fake clock for policy/timer tests. Run `go test ./...`,
`go vet ./...`, native supported race checks and frontend type/interaction checks
against each affected build. Build headless and desktop separately. Native test
drivers are opt-in and must not change firmware, sleep policy or global network
settings. Physical sleep/auth/credential/network evidence is required in addition
to mocks and source inspection.

| Parent obligation | Owning slices and integrated acceptance |
| --- | --- |
| Requirements 1–2: activity requests/releases sleep prevention | 1–4 own state/controller/native request; 12 proves actual Windows idle/lock/display behavior; 16 and 18 prove ports |
| Requirement 3: generic/T3/HTTP/power plus logical combinations | 2 owns logic; 3/5/6 own adapters; 12 verifies Windows/WSL with actual T3 work |
| Requirement 4: per-probe errors and defensive default | 1/2/5/6 own policies/observations; 8/9 explain them; 12 verifies outage/recovery and mixed expressions |
| Requirement 5: native UI/TUI/status/history/simple controls | 4/7/8/9; 12 verifies manual zero-setup/timed mode, monitoring distinctions, diagnostics and access |
| Requirement 6: complete Windows before Mac/Linux | 12 is a hard dependency for 13; 16 is a hard dependency for 17 |
| Requirement 7: Mac closed-lid on AC/battery | 13 owner-deferred hardware/mechanism decision; 15 conditional implementation; 16 physical acceptance; no idle-only substitution |
| Requirement 8: authorized wake, remembered targets and trust separation | 10/11 own shared network/page/auth; 12/16/18 verify actual target wake and no management bypass |
| Prose: battery limits, startup/reset, display/locking | 2/3 own common/Windows behavior; 14/15/17 own native additions; complete platform slices verify actual outcomes |
| Prose: portability, safe credentials/config, diagnostics, startup | 1/3/4/8 own shared/Windows behavior; 12/14/16/17/18 port/package/verify |
| Selected UX and unconditional timed controls | 2 owns semantics; 7/8/9 implement selected native/text flows; 12 verifies keyboard, scaling, state and release |

Five integrated acceptance journeys follow the HLD: unattended local native/WSL
T3 work to settled idle; unreadable active source with retain/release/recovery;
manual/timed/monitoring/battery/OS failure; real Mac closed-lid work and recovery;
and travel remote wake with remembered sleeping targets and authorized access.
Each platform record identifies OS/hardware/power state/dependency versions,
steps, expected/observed results and limitations. No platform is delivered from
compile-only or fake tests. Stop on a requirement invalidating result and return
the evidence to its governing design decision.

## Size and current readiness

Estimates are incremental final delivered nonblank physical lines, excluding
comment-only lines. Count docstrings and multiline statements physically.
Production includes shipped code/UI/configuration/packaging/instructions. Tests
include test-only helpers/configuration/generators/integration drivers. Exclude
design documents, static fixture data, images, generated files, lockfiles,
dependencies and disposable probes. Do not use added-plus-removed edit churn.
Shared foundations/tests are counted once in their owning Windows prerequisite.
Every canonical slice has separate inclusive production/test line ranges.

Project envelope: **10,850–17,450 production lines** and **12,000–19,400 test
lines**. This includes a conditional Mac closed-lid envelope of 800–1,600 production
and 900–1,800 test lines, not a chosen mechanism or a ready implementation task.
The design investigation counts zero shipped/test code because it delivers
planning evidence. Estimates have greenfield and native/hardware uncertainty;
real evidence can require an affected reviewed replan. There is no agreed global
slice cap, time estimate or new accounting mechanism.

Windows slices 1–12 are detailed for review, subject to their explicit engineering
and physical acceptance prerequisites. Product execution remains unauthorized.
Slice 13 requires completed Windows and real Mac access. Slices 15–16 are not ready
until the retained Mac mechanism decision, updated engineering and renewed affected
review/approval exist. Linux slices 17–18 depend on complete Mac delivery. The
whole-project design must preserve this partial readiness rather than claim a
finished ready-to-implement stack or quietly omit later platforms.
