# SnooZap high-level design

Status: the initial architecture and selected UX are owner-approved. After the
name-only rename, the owner approved the WSL process-check lifecycle amendment
described below. Original exact-byte stamps remain historical evidence;
current review evidence is recorded separately. No implementation is authorized.
See [rename provenance](supporting/rename-notes.md) and the
[WSL investigation and owner decision](supporting/implementation-plan/wsl-lifecycle-evidence.md).

Governing needs: [approved requirements](requirements.md).
Evidence and open feasibility checks: [architecture research](supporting/high-level-design/research.md).

## Architecture and delivery

Use Go for the shared runtime. Use Wails v3 for the desktop tray/menu-bar item
and its local window, and Bubble Tea for the TUI. Pin verified stable dependency
versions during detailed planning. The UI uses bundled assets in the OS webview;
it needs no Node.js server at runtime. Go remains the primary language, with
small platform bridges where native APIs require them.

One runtime per OS user owns probe scheduling, rule evaluation, configuration,
history and the local OS sleep request. The desktop app normally owns that
runtime. A headless console invocation can own the same engine instead. A TUI
attached to an existing runtime reads and controls that engine; it does not poll
applications or acquire another sleep request independently.

| Responsibility | Boundary |
| --- | --- |
| Probe adapters | Read local process, T3, HTTP and power observations |
| Rule evaluator | Combine observations and explain matches or uncertainty |
| Hold controller | Apply user mode and battery policy; own one OS request |
| Desktop and TUI | Present the same snapshots and submit local user commands |
| Native local IPC | Connect the CLI/TUI to the owning runtime, restricted to its OS user |
| Optional wake component | Discover and remember LAN targets; expose wake-only operations |
| Platform adapters | OS power requests, process inventory, credentials and lifecycle |

Deliver complete Windows workflows first, including Windows-to-WSL T3 access
and Windows wake hosts/targets. Deliver macOS next, including verified closed-lid
behavior, and Linux last. The Windows phase does not ship a Linux SnooZap helper.
Fixed WSL inspection operations are Windows integration, not a second Linux
product release.

The core requires no administrator service, LAN listener or HTTP control server.
T3 OAuth setup can temporarily open a loopback-only sign-in callback. Local CLI
communication uses a user-restricted Windows named pipe or Unix socket.
The optional wake component has separate network handlers and cannot route a
remote request into the local control interface.

Windows packaging includes a tray executable and a console executable from the
same engine. This accommodates Windows GUI/console subsystem behavior. macOS
packages an app bundle and CLI; Linux packages the corresponding desktop and
console entry points. A headless build excludes desktop dependencies.

Portability means builds for each supported OS and architecture. Desktop builds
still depend on the platform webview and Linux desktop libraries. The Windows
portable archive requires no machine-wide installation for core use. Autostart
is opt-in. Exact minimum OS versions and Linux desktop support remain part of
the verification matrix, not a claim of universal compatibility.

## Observation and rule behavior

Each probe reports `active`, `inactive` or `unknown`, with an observation time
and a bounded error category. Unknown includes timeout, expired authorization,
unsupported schema, access denial and an incomplete process enumeration.
An error never becomes a successful inactive observation.

Initial probes:

- **Application running:** match an explicitly selected executable or exact
  process name, scoped to the local user. A Windows process selection can also
  name a WSL distribution. WSL inspection uses fixed built-in operations through
  `wsl.exe`, without a user command template or a shell. Before each inspection
  command, check the selected distribution's running state. If it is stopped,
  missing or cannot be classified, skip execution and report unknown. This is
  best effort: a distribution can stop after that check, and the inspection
  command can then restart it. The owner accepts this race. Never shut down or
  terminate a distribution to undo a possible restart; another application may
  already be using it. Explain this limit when selecting a WSL application and
  in its settings. Missing inspection capability is an error, not proof that
  the application ended. T3 polling uses its existing authorized local endpoint;
  it does not launch a WSL process-inspection command for each poll.
- **T3 activity:** read the selected local environment through authenticated
  MCP. Count `preparing`, `queued`, `starting`, `running` and `waiting`; include
  child agents and all projects. Staging states prevent an avoidable gap before
  `running`. Exclude settled statuses. This measures chat state, not detached
  background commands after a chat settles.
- **HTTP condition:** GET a user-configured local endpoint. Require a successful
  HTTP status and evaluate a typed JSON value selected by JSON Pointer. Support
  equality, inequality, numeric comparisons and existence. Missing required
  fields or type mismatches are unknown. This provides application-defined logic
  without executing user scripts.
- **Power source:** report whether the machine is on AC or battery for use in
  rules. Battery safety limits are also enforced separately from these probes.

T3 setup selects the native local environment or a local WSL distribution,
connects to its loopback MCP endpoint, and uses T3's external read-only OAuth
approval. Do not use this agent's credentials, scrape the state database or
infer locality from the T3 window's existence. A remote saved T3 environment is
not an eligible local probe. External OAuth and endpoint identity still need an
end-to-end Windows/WSL verification before this integration can be called ready.

HTTP probes also accept only loopback endpoints. Resolve and validate the actual
destination; disable redirects and proxy-based routing for this local check.
Credentials are references to the OS credential store, not inline configuration
secrets. Do not accept URL user information. Default polling is every 5 seconds,
with a 2-second timeout and a 1 MiB response limit. One request per probe may be
in flight. T3 protocol negotiation/authentication has a separate bounded setup
deadline; its polling timeout must not interrupt an interactive sign-in.

Rules form finite trees of probe references, `all` (AND), `any` (OR) and `not`.
There are no executable expressions or recursive rule references. Independent
enabled rules combine with OR to request an automatic hold. Configuration must
reject missing references, empty operators and excessive nesting.

Evaluate observations with three-valued logic:

| Expression | Observation result |
| --- | --- |
| `NOT unknown` | unknown |
| `false AND unknown` | false |
| `true AND unknown` | unknown |
| `true OR unknown` | true |
| `false OR unknown` | unknown |

Track separately whether a rule is retaining an earlier hold. A confirmed true
result requests a hold. Confirmed false releases that rule's hold immediately.
An unknown result retains an existing rule hold only while every failed probe
contributing to that uncertainty permits retention. A decisive known branch
removes irrelevant uncertainty: a known true OR branch remains sufficient even
if another branch has failed.

The per-probe default is `retain`: no failure deadline releases an existing
automatic hold. The alternative is `release_after`, with a configurable grace
period, proposed default 60 seconds. Its deadline starts at the first failed
observation and resets after a successful observation. When any contributing
probe's deadline expires, an uncertain rule releases its retained hold. With
mixed policies in an uncertain OR expression, this is deliberately conservative
about retention: all contributing unknown probes must still allow it. Show the
expired policy as the reason. A different independently confirmed rule can
still hold the machine awake.

At startup there is no previous automatic hold to retain. Unknown alone does
not acquire one by default. Show “Not confirmed — no automatic hold” until a rule
is confirmed true. Keep this configurable per probe as `on_startup_unknown`, with
`wait` as the default and `hold` as the alternative. For an unknown rule with no
previous hold, the alternative acquires an unconfirmed hold only when every
probe causing that uncertainty has no successful observation in the current
monitoring session and is configured to hold at startup. A known false rule
still releases; a known true rule still holds. Show startup-policy protection
separately from confirmed activity.

Once a probe has a successful observation, later errors follow `on_error`.
Existing rule holds follow the previously described retention policy; a startup
`wait` setting does not veto an already established hold. Do not restore old
observations after restart. Re-enabling monitoring or changing a probe definition
starts fresh observations for the affected probes and rules. Users can select
Stay awake independently while repairing a connection.

On resume, invalidate pre-sleep observations and poll again. An existing
retained rule state may protect work during this refresh under the same failure
policies. A new OS request is acquired only after current mode and battery
checks. OS notifications trigger prompt reconciliation; a periodic check catches
missed events. There is no simulated input and no repeated OS-setting rewrite.

## User controls, setup and diagnostics

The tray/menu-bar icon opens a local window with Status, Rules, History and
Settings. Status shows observed activity, the reason for the requested hold,
whether the OS accepted that request, and any support limitation. Color is
supplemented by text and icon shape. Closing the window leaves the tray runtime
running. Quit releases SnooZap's requests and stops the runtime.

Expose manual controls directly in the tray/menu-bar menu, without opening the
window or editing a rule: **Stay awake**, **Stay awake for…**, **Auto**, and
**Allow sleep**. Stay awake acts immediately with no time limit until changed
or SnooZap exits. The timed action offers 15 minutes, 1 hour, 4 hours and a custom
positive duration. The local window provides the same controls. The TUI exposes
them directly with `w` (Stay awake), `t` (timed), `a` (Auto), and `s` (Allow sleep).
In text-entry dialogs those keys type text rather than triggering a mode change.

Manual Stay awake is unconditional with respect to probes, rules and the
monitoring toggle: it works with no probes configured, with monitoring off, or
when every probe is inactive or unknown. It uses the same hold controller and
native OS request, rather than a second prevention mechanism. Configured battery
limits and OS restrictions still apply and remain visible.

Show the active manual mode and remaining time in the menu/status panel and
TUI. Timer expiry returns to Auto; it does not issue Sleep. An automatic rule
or the wake-host role may therefore keep the machine awake after expiry, and
the displayed reason must change accordingly. A new manual action replaces the
previous manual timer; selecting an untimed action clears it. Expiry during
system sleep is handled on resume before reacquiring any expired manual hold.

Modes:

| Control | Behavior |
| --- | --- |
| Auto | Apply enabled rules and the optional wake-host hold |
| Stay awake | Request an immediate manual hold, optionally timed; offer 15 minutes, 1 hour, 4 hours, custom, or until turned off |
| Allow sleep | Release all SnooZap holds while continuing observations |
| Monitoring off | Stop observations and automatic rule holds; a manual or wake-host hold can remain |

The mode chooser must explain that Allow sleep releases SnooZap's request rather
than issuing an immediate Sleep command. Manual modes last until their timer
expires, the user selects Auto, or the runtime exits. They reset to Auto on
restart. Monitoring enabled/disabled is a saved setting. Re-enabling monitoring
starts fresh observations rather than trusting paused results.

Battery operation is enabled, with a proposed 20% floor and recovery at 25% or
on AC. Expose both thresholds and an AC-only option. The battery guard overrides
rules, manual mode, wake hosting and closed-lid mode. If power telemetry becomes
unreadable on a battery-capable machine, release holds and show the reason;
probe failure retention cannot override this guard. A platform-confirmed absence
of a battery is different from failed telemetry. Exact telemetry APIs and
notification behavior belong to detailed planning and platform verification.

First launch has no implicit process-presence rule. “Add T3” connects and tests
the local environment, then creates one enabled T3 rule. “Add application”
selects the process; advanced rule editing exposes AND/OR/NOT and HTTP conditions.
A newly created probe displays its failure policy. A Test action uses the same
probe adapter and reports a bounded sanitized result without creating another
sleep request. There is no automatic remote access setup.

Keep the latest 500 API-call records and 500 state/control events in memory.
Record time, probe ID, operation, duration, result category and protocol/HTTP
status when safe. Do not record raw headers, request bodies, response bodies,
URL query strings, OAuth codes or tokens. History clears on restart or through
Clear history; users can explicitly export the sanitized history. Credential
storage failure blocks credentialed probe setup rather than falling back to
plaintext secrets.

Local configuration uses versioned JSON and an OS-user-restricted data folder.
GUI and TUI edit the same configuration through the owning runtime. Validate
the full candidate before atomically replacing the active configuration. A bad
edit preserves the running valid configuration and displays its error. A bad
configuration at startup does not invent a default keep-awake rule. Unknown
schema versions are rejected. A migration retains the original file and requires
confirmation before changing public behavior. No legacy config exists yet.

Representative advanced configuration, omitting default tuning fields:

```json
{
  "schema_version": 1,
  "probes": {
    "t3": {"type": "t3", "local_environment": "selected-local", "on_error": "retain"},
    "render": {"type": "process", "name": "blender.exe", "on_error": "retain"},
    "ac": {"type": "power", "source": "ac", "on_error": "retain"}
  },
  "rules": [
    {"id": "work", "when": {"any": [
      {"probe": "t3"},
      {"all": [{"probe": "render"}, {"probe": "ac"}]}
    ]}}
  ]
}
```

This example holds for T3 work on either power source, or for rendering on AC.
Battery limits still apply. Independent rules combine with OR: adding a narrow
rule cannot restrict an existing broader rule. The UI shows all matching reasons.

Proposed CLI shapes:

```text
snoozap run --tui
snoozap tui
snoozap status [--json]
snoozap mode auto|awake|allow-sleep [--for 15m]
snoozap monitor on|off
snoozap config check|apply|export <path>
snoozap history [--json]
snoozap wake list
snoozap wake send <remembered-target-id>
snoozap stop
```

`run --tui` owns a foreground runtime; exiting it releases its holds. `tui`
attaches to an existing runtime; detaching leaves it running. Other commands
require the runtime except offline `config check`. They do not silently start
monitoring. A second owner invocation reports the existing instance. Optional
`--data-dir` chooses storage, not permission to run competing owners in the same
OS-user session. `--for` is allowed only with `awake` and `allow-sleep`; omitting
it means until the user changes mode or the runtime exits. Values must be positive
durations. Expiry returns to Auto. `stop` gracefully stops the owning runtime,
including its wake listeners, rather than merely hiding a window.

CLI exit codes are 0 for a successful command, 2 for invalid input or config,
3 for no running instance, and 1 for an operational error. A status command
reports successful observation of the engine even when a probe is unknown or
the OS rejected a request; the JSON carries those failures. It must not use a
zero process exit code to imply that work is protected.

The version-1 configuration's public fields are:

| Object | Fields and defaults |
| --- | --- |
| Root | `schema_version: 1`, `probes: {}`, `rules: []`, optional `settings` |
| Every probe | `type`, `enabled: true`, `on_startup_unknown: "wait"` (alternative `"hold"`), `on_error: "retain"`, `grace_seconds: 60` when `on_error` is `"release_after"`, `interval_ms: 5000`, `timeout_ms: 2000` |
| Process | Exactly one of `name` or `executable`; optional `wsl_distribution` on Windows |
| T3 | `local_environment`: an ID created by Add T3 setup; it resolves to the authorized local endpoint and credential reference, never a remote environment |
| HTTP | `url`, `pointer` (default empty/root), `operator` (default `"equals"`), `value` (default `true`); optional `credential_headers` mapping header names to OS credential references |
| Power | `source: "ac"` or `"battery"` |
| Rule | `id`, `enabled: true`, `when`: exactly one of `{"probe": "id"}`, `{"all": [...]}`, `{"any": [...]}`, or `{"not": expression}` |
| Settings | `monitoring: true`, `battery: {"ac_only": false, "floor_percent": 20, "resume_percent": 25}`, `autostart: false`, `closed_lid: false`, `wake: {"target": false, "host": false, "remote": false, "https_port": 8443, "interface": null}` |

An exported T3 environment ID is machine-specific. Export its non-secret local
ID only, without its endpoint binding or OAuth material. Import on another
machine requires that ID to be rebound through local setup; no automatic remote
fallback is permitted. The setup registry and credentials stay in the protected
per-user data directory. HTTP credential references likewise require local
binding. Plaintext credential import and export are not supported.

Reject unknown config fields, duplicate JSON keys, invalid references and
incompatible probe fields. Require positive polling/timeout durations, timeout
shorter than interval, and positive grace for release-after. Require
`0 < floor_percent < resume_percent <= 100`. HTTP operators are `equals`,
`not_equals`, `greater_than`, `greater_or_equal`, `less_than`, `less_or_equal`
and `exists`. Comparisons do not coerce JSON types. `exists` returns inactive
when the pointer is absent; missing values for other operators are unknown.
Equality supports JSON scalar values; ordered comparisons require numbers.
Empty `probes` or `rules` is valid and creates no automatic hold.
Wake setup binds `interface` to the selected physical LAN adapter; if multiple
adapters are eligible, the user chooses one. Missing or replaced adapters make
wake unavailable until rebound. `remote` requires `host`; enabling it does not
apply the displayed Serve command. Validate HTTPS port values and show a setup
error if the installed Tailscale does not support the selected value.

`snoozap status --json` uses this public shape; timestamps are RFC 3339 or null:

```json
{
  "schema_version": 1,
  "mode": "auto",
  "monitoring": true,
  "activity": "unknown",
  "hold": {"requested": true, "os_accepted": true, "reasons": ["retained:work"]},
  "battery": {"source": "ac", "percent": null, "blocked": false},
  "limitations": [],
  "probes": [{"id": "t3", "state": "unknown", "observed_at": "2026-11-03T00:00:00Z", "error_code": "unreachable"}],
  "rules": [{"id": "work", "state": "unknown", "retained": true}]
}
```

Activity and rule states are `active`, `inactive` or `unknown`; activity is the
three-valued OR of enabled rules while monitoring, and unknown while monitoring
is off. Hold reasons identify `rule:<id>`, `retained:<id>`, `manual`, or
`wake_host`. `requested` is the desired request after overrides and battery
policy. `os_accepted` describes the current API request, not an assurance against
platform restrictions. An OS failure is represented by `os_accepted: false` and
a non-secret limitation/error description. Battery source is `ac`, `battery`
or `unknown`; percent can be null, including confirmed battery-less systems.
Probe error codes are bounded categories, not raw remote text. JSON history is
`{"schema_version": 1, "records": [...]}` with the sanitized record fields
described above. Version-1 response readers ignore additional fields; incompatible
meaning or type changes require a new schema version. Secret values are absent
from all status, config export and history responses.

The status example assumes a previous confirmed match followed by a failed
poll. `observed_at` is the last completed attempt time, including failed attempts;
it is null before any attempt. Last-success timing is a separate diagnostic.

## Native sleep requests and platform limits

On Windows, use the handle-based PowerCreateRequest/PowerSetRequest/
PowerClearRequest APIs with SystemRequired, a readable reason and balanced
lifetime management. Do not use DisplayRequired or AwayMode for normal operation.
This avoids binding the Go policy engine to a particular execution thread.
Process exit closes the handle. An API success means that Windows accepted the
request; it does not prove that a policy or hardware limitation cannot end it.

Explicit Windows sleep and lid actions remain outside ordinary idle prevention.
Modern Standby on battery has a documented request-duration limitation. Detect
the power model and show a persistent support limitation in that combination;
do not renew requests merely to evade OS policy or label it continuous protection.
Verify traditional sleep and Modern Standby separately. SnooZap does not alter
global sleep timeouts, lid policy, NIC configuration or firmware automatically.

On macOS, ordinary idle protection uses IOKit power assertions. The owner has
explicitly deferred the closed-lid mechanism decision until investigation on a
real Mac. Continued closed-lid work remains required. A privileged helper
controlling the system-wide SleepDisabled setting is a candidate only; neither
that component nor its installation or effects on manual Sleep are approved.

The first Mac work must investigate the actual hardware/OS behavior and compare
available mechanisms. Test AC and battery operation, power-source changes,
locking, explicit sleep, failure recovery and coexistence with other sleep
utilities. Return the findings, support limits and any required privilege or
installation tradeoff to the owner before selecting and implementing the
mechanism. Do not describe ordinary idle assertions as proof of lid-closed work,
or silently substitute an external-display-only setup.

If the helper candidate is revisited, evaluate authenticated narrow operations,
independent battery cutoff, bounded leases and recovery after app/helper failure
or reboot. Avoid a general passwordless sudo rule. Its setting is machine-wide;
identical writes from concurrent utilities cannot reliably be attributed, and
the override may prevent manual Sleep. These are investigation criteria, not
claims of an implemented or accepted recovery design.

The proposed `closed_lid` setting remains off and unavailable until that decision
is settled. The whole-project plan must retain the Mac investigation and the
dependent closed-lid implementation/verification work, with that portion marked
not ready. This owner-approved timing deferral permits Windows design to continue;
it does not defer or remove the required Mac outcome.

On Linux, use systemd-logind's native inhibitor facility on supported systems,
with file-descriptor lifetime and truthful permission failures. The desktop
session's idle-suspend policy must be tested too; a logind request alone is not
proof that every desktop honors the desired distinction between display sleep,
idle suspend and explicit Suspend. Select and verify the narrow inhibitor types
during Linux planning. Native tray availability also depends on the desktop.

## Optional LAN discovery and remote wake

The wake feature is off by default. Settings distinguishes “Make this computer
a wake target” from “Host the wake page.” A target advertises wake metadata while
awake, but does not acquire a permanent hold. A host discovers targets and adds
an explicit Wake host hold so it remains available. Allow sleep and the battery
guard can override that hold; the UI then warns that remote wake availability
can be lost. Losing the host does not make another sleeping instance reachable.

Use standard mDNS/DNS-SD for same-link discovery, with a versioned SnooZap service
and a persistent installation ID. Wake metadata contains only a display name,
installation ID, protocol version and relevant physical adapter MAC information.
Do not advertise credentials, rules or activity. Treat names and metadata as
untrusted input, not HTML, paths or routing instructions.

Remember discovered targets until explicitly forgotten, including their last
seen time, adapter and local network association. Bound the saved list to 256;
when full, warn and stop adding new targets rather than silently removing older
sleeping ones. Do not broadcast on VPN, WSL virtual or unrelated interfaces.
A remembered target from another LAN is unavailable until it is discovered on
the selected current LAN. Exact network identity and multi-adapter handling need
LLD resolution before implementation readiness.

The LAN listener and the remote-facing backend expose only wake metadata and
wake operations. Same-link LAN wake needs no pairing. A LAN handler accepts
only traffic from the selected connected network, ignores forwarded identity
headers, and never exposes the local IPC operations. Endpoint binding, firewalls
and source classification must be checked on each supported host platform.

Remote exposure uses the user's installed Tailscale and Tailscale Serve, with
an isolated loopback wake-only backend. SnooZap does not embed another Tailscale
node. Remote setup displays one Serve command for a dedicated HTTPS port,
proposed default 8443, plus the matching tailnet-access guidance. The user runs
the command; SnooZap does not rewrite or reset Serve configuration. Check for
an existing mapping on that port and refuse to suggest replacing it; selecting
a different free port requires updating the displayed access rule and URL.
The backend binds only loopback and uses a separately allocated local port.
Existing Serve services therefore need no app-managed migration or rollback.
Use tailnet access rules for authorization. The backend requires Serve-provided
user identity and denies requests without it; do not enable Funnel. Tagged
client devices without user identity are not supported by this initial browser
flow. No second SnooZap account or peer-pairing database is required.

The setup UI must explain that anyone allowed by the tailnet's policy to access
this wake service can use its wake controls, including shared-device users if
the policy permits them. Membership alone is not an assertion that every node
has one owner. A distinct LAN listener must not act as a bypass into the remote
authorization route or into local diagnostics.

The browser page lists remembered targets and has one Wake button per target.
Its proposed protocol surface is:

```text
GET  /v1/targets
POST /v1/targets/{id}/wake
GET  /v1/wake-requests/{id}
```

All responses carry `schema_version: 1`. Target listing returns `targets`, each
with `id`, `name`, `last_seen_at` and `reachability` (`reachable`, `not_seen` or
`different_network`). A wake POST has an empty body and returns HTTP 202 with
`request_id` and `state: "packet_sent"` after sending; an already reachable
target returns 200 and `state: "already_reachable"`. Polling that request returns
its ID, state and creation/update timestamps. States are `packet_sent`,
`target_reachable`, `already_reachable`, `unconfirmed` and `send_failed`.
Errors return `error.code`: 400 invalid input, 403 denied, 404 unknown target or
request, 409 wrong network, 429 rate limit, or 503 unavailable/send failure.
Request records are held in a bounded in-memory history; after restart or
expiry, 404 means the record is unavailable, not that no packet was ever sent.
Do not automatically retry an uncertain POST. Repeated explicit Wake actions
are subject to the same target rate limit. Incompatible protocol versions are
shown as unsupported; they are not decoded as version 1 or granted new authority.

The POST accepts no arbitrary destination, MAC, port or command. It can wake
only a remembered target on the selected network. Use same-origin browser
requests, anti-CSRF protection and bounded per-client/target rate limits. The
web page and handlers have no configuration or diagnostic routes.

Send a bounded WOL packet sequence from the host to the remembered physical
adapter. Report `packet_sent`, then `target_reachable` if the target's matching
SnooZap instance reappears, or `unconfirmed` after a proposed 60-second timeout.
An already reachable target is reported as such. A packet send failure is
`send_failed`. These are reachability observations, not proof that a user session
is unlocked or T3 is ready. LAN discovery is not authenticated, so it does not
establish a cryptographic identity guarantee.

Verify each target's actual NIC, firmware, OS and sleep-state support. Do not
promise Wi-Fi wake, shutdown wake or Mac closed-lid wake from a successful UDP
send. Hardware setup guidance is explicit; the app does not automatically flash
a router or change target firmware. The user's Linux media PC can become a host
after Linux delivery, not through an implicit early helper exception.

## Journeys and evidence required before acceptance

1. **Unattended T3 work:** Add and authorize local T3; a running or waiting chat
   activates the rule; the controller acquires the native request. The tray and
   attached TUI show the same reason. Settled work releases the hold. Verify this
   on Windows with a T3 environment in WSL, allowing screen-off and normal lock.
2. **Unreadable source:** A previously active T3 probe fails. Show unknown and
   the retained hold. Under release-after, expiry releases the uncertain rule;
   a separately confirmed matching rule still holds. Recovering idle releases
   the retained hold. Test nested AND/OR/NOT and mixed probe policies.
3. **User and battery controls:** Allow sleep overrides every SnooZap cause;
   monitoring off has the narrower documented meaning. Start a manual hold from
   the tray and TUI with no probes and with monitoring disabled. Verify untimed
   and timed actions, countdown, timer replacement, expiry returning to Auto,
   and expiry while asleep. A battery floor overrides even manual and wake-host
   holds. OS request failure is visible and distinct from a matching rule. Verify
   resume, logout, runtime crash and invalid edits.
4. **Closed Mac lid:** On a declared supported Mac, run real work with the lid
   closed on AC and battery. First investigate and obtain a mechanism decision
   on real hardware, then verify battery cutoff, any privilege denial, component
   failures, reboot recovery, power changes, locking and conflicts with another
   sleep utility. Do not mark Mac delivered until this requirement works.
5. **Travel and remote wake:** Discover a target while it is awake, sleep it,
   reach the still-awake host through authorized Tailscale, press Wake, and
   distinguish send from reachability. Test denied remote callers, unsupported
   target wake states, unavailable host, LAN change and a full remembered list.

The remaining engineering investigations belong to detailed planning and release
verification. They include T3 OAuth/local-environment identity from Windows/WSL,
credential storage on each OS, desktop/headless packaging, authenticated local
IPC, the deferred Mac closed-lid mechanism and hardware support, Linux desktop
inhibition and wake hardware/network behavior. Native IPC is an internal versioned
interface: mismatched versions refuse attachment with an upgrade message instead
of starting a competing runtime. It grants no remote network access.

For each investigation, the implementation plan must name the prerequisite,
observable check and affected delivery gate. No platform is delivered on source
inspection alone. Failure that invalidates a selected mechanism returns to the
affected architecture decision; it is not permission to drop a parent outcome.
Source evidence alone does not establish platform behavior. Hardware verification
remains required before claiming a delivered platform outcome.

Review this draft against the requirements, then present its actual controls,
startup/failure behavior, interfaces, defaults, privilege and platform limitations
inline for explicit owner approval. Only then prepare implementation slices and
estimates. Neither a clean HLD review nor approval is implementation readiness.
