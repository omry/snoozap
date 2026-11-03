# SnooZap requirements

Status: owner-approved requirements, renamed to SnooZap at the owner's request.
The pre-rename review and approval evidence is retained; its exact-byte stamp
does not attest this name-only revision. Requirements approval does not authorize
implementation. See [rename provenance](supporting/rename-notes.md).

## Purpose

Keep the local computer awake while selected work needs it, and allow normal
sleep when that work no longer needs the computer. Show why sleep is being
prevented and let the user control that behavior.

## Planning scope and delivery order

Design the whole agreed project. Deliver Windows first, then macOS, then Linux.
Complete and verify each platform through its user-facing workflows before
moving to the next platform. macOS includes continued work with the lid closed.
Remote wake is an optional capability with external prerequisites.

The repository is public at https://github.com/omry/snoozap. Go is a candidate
implementation language; it is not yet an accepted architecture decision.

## Activity and conditions

- Monitor work on the local machine, including work hosted in WSL on Windows.
- Support a generic condition that is active while a selected application runs.
- Support application-specific conditions for meaningful work state.
- Start with a specialized T3 condition. Running and waiting chats count as
  activity; an open T3 application alone does not establish active chat work.
  Count only work belonging to the local environment, including its local child
  agents; access to another machine through T3 does not itself make that remote
  work local activity.
- Let users combine conditions with AND, OR, and NOT.
- Keep detection state distinct from the resulting sleep-prevention state.
- Provide configurable built-in probes, including HTTP/API conditions through
  which users can express their own application-specific checks. This is an
  initial supported extension mechanism; arbitrary command/script probes are
  outside the current agreed scope.
- Include power-source conditions for use in logical combinations.

The exact rule configuration interface and HTTP condition contract remain
architecture work. T3 may start as a specialized integration; whether it can
reuse the generic HTTP machinery depends on its actual protocol and authentication.

## Monitoring failure

Behavior when a monitored source cannot be read must be configurable per probe.
Support the discussed alternatives: retain sleep prevention while activity
remains unconfirmed, or release sleep prevention after a grace period. Provide
a defensive default.

The defensive default protects potentially active work by retaining sleep
prevention when a previously active probe becomes unreadable.

A failed observation does not establish that work has finished. Show the
monitoring failure separately from a successful inactive observation. Exact
startup behavior, timeout settings and interaction with logical combinations
remain public-contract design work. A condition failure inside a NOT expression
must not be silently treated as a trustworthy true or false observation.

## User experience

- Keep normal setup and operation simple.
- Provide a persistent native status indicator appropriate to the OS, such as
  a Windows notification-area icon or a Mac menu-bar item.
- Open a UI from that indicator with current detected activity, the effective
  sleep-prevention state, useful diagnostic information and API-call history.
- Let the user enable or disable activity monitoring and sleep prevention.
- Support text operation with a TUI.
- Make UI and TUI explanations agree about activity, overrides and failures.

The exact control labels, manual override behavior, diagnostic retention and
configuration surfaces will be presented in the architecture's public contract.

## Platform behavior

Use OS facilities to request sleep prevention. Routine idle-sleep prevention
must allow the display to turn off. Do not assume that an ordinary idle-sleep
request establishes closed-lid operation on a Mac.

Declare supported platform and hardware conditions, show failures truthfully,
and verify the actual behavior on each target platform. Support sleep prevention
on battery as well as while plugged in, with configurable battery limits. This
includes Mac closed-lid operation where technically supported. The exact default
battery policy, thresholds, closed-lid support conditions and any privileged
installation requirements remain architecture and engineering work.

## Optional remote wake

Provide an optional wake-hosting role. An enabled, awake SnooZap instance hosts
a small web interface, reachable remotely through authorized Tailscale access.
The interface lists SnooZap peers discovered on its LAN and provides an explicit
Wake action. Remember previously discovered targets and their network-adapter
addresses so that sleeping peers remain available as wake targets.

SnooZap instances on the same LAN discover one another and may request wake
without a separate pairing ceremony. That local trust grants wake access only.
It does not grant remote access to diagnostics, probe configuration, credentials
or settings. Authorize remote requests separately through existing Tailscale
access rules. Conventional hardware Wake-on-LAN is not sender-authenticated;
SnooZap permission checks cannot prevent direct wake packets from other LAN peers.

At least one suitable LAN instance must stay awake and reachable. Targets need
supported, enabled Wake-on-LAN. Send the hardware wake packet from the awake
instance; a sleeping target's SnooZap process does not receive the web request.
Distinguish a packet being sent from the target becoming reachable, and report
timeouts and failures truthfully.

The remote page provides wake controls only. The broader diagnostic and
configuration UI remains local. Automatic waking from an ordinary T3 or other
remote-control connection attempt is outside the current scope. Existing
external Wake-on-LAN tools remain possible alternatives, not required second
implementations within SnooZap.

Supported helper platforms, discovery protocol, remembered-target lifecycle,
hardware wake states and post-wake checks remain engineering work. Mac
closed-lid wake must be verified separately from idle-sleep prevention.

## Normal operation and boundaries

A user configures probes and logical conditions on the local machine. SnooZap
observes the selected activity, evaluates the conditions and explains whether
it currently requests sleep prevention. The user can disable monitoring or
sleep prevention. When the conditions no longer require a hold, SnooZap releases
its request and the OS applies its normal sleep policy. Releasing a hold does
not mean issuing an immediate Sleep command.

Keep local probe execution, local user controls and OS sleep control separate
from the optional wake-facing interface. Discovery and wake authorization must
not turn the application into a remote probe runner or expose probe credentials.
Remote wake applies to discovered SnooZap targets under declared LAN conditions;
it does not create a general remote-management system.

SnooZap must report the effective result, including observation errors, user
controls, battery limits and OS request failures. Do not claim a machine is
protected merely because a rule matched, or claim a target woke merely because
a packet was transmitted. Diagnostics and API history must avoid exposing
credentials. Normal sleep prevention must not unlock the machine.

## Observable acceptance

1. A matching local activity condition causes an OS sleep-prevention request.
2. A successful observation that activity ended allows normal idle sleep,
   subject to other conditions and the user's selected controls.
3. Generic application presence and T3 chat activity can be combined logically.
   Configurable HTTP/API conditions and power-source conditions participate in
   the same rule behavior.
4. A monitoring failure follows the configured policy and is visible as a
   failure, rather than being reported as confirmed inactivity.
5. The native UI and TUI expose consistent detection and sleep-prevention state,
   diagnostic history and effective user controls.
6. Windows behavior is verified end to end before subsequent platform delivery.
7. On supported Macs, work continues with the lid closed under the declared
   operating conditions; unsupported conditions are identified.
8. With a configured reachable helper and supported target, an authorized remote
   wake request wakes the target and reports success or failure truthfully.
   Previously discovered sleeping targets remain visible, and local discovery
   grants no diagnostic, configuration or credential access.

## Remaining owner decisions and engineering work

All four interview topics and the complete requirements summary, including the
defensive failure default, have received explicit owner approval.
The architecture must present the remaining concrete public behavior and defaults
for its own approval, including settings, limits, startup and failure cases.

Engineering work includes external T3 authentication, host-to-WSL access,
precise status semantics, logical evaluation with unknown observations,
OS-specific sleep prevention and recovery, UI/TUI integration, packaging,
startup, credential protection, diagnostic redaction, optional wake-host discovery
and authorization, and verification of closed-lid and Wake-on-LAN behavior.
These are finite investigations for architecture and detailed planning; a
question being answered does not establish requirements approval or
implementation readiness. [Interview and research evidence](supporting/requirements/interview-evidence.md)
is context, not a substitute for owner approval or platform verification.

## Current exclusions

This workflow produces requirements, architecture and an implementation plan.
It does not implement or install the product, configure services or firmware,
publish documents, or change machine power or network settings. No product code
or platform behavior has yet been delivered.
