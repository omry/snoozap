> Historical pre-rename evidence for SnooZap (formerly Dozolt). Names, decisions
> and review/hash observations below describe that earlier state. See
> [rename provenance](../../supporting/rename-notes.md).

# Provisional architecture evidence

Research date: 2026-11-03. This is supporting context for the provisional HLD,
not an authority, implementation report or completed platform test.

## Desktop and text interfaces

[Wails API documentation](https://wails.io/reference/overview/) lists native
desktop integration and OS webviews. The [official release list](https://github.com/wailsapp/wails/releases)
lists stable v3.1.0 separately from a v3.2 nightly. This supports evaluating a
stable v3 desktop frontend rather than requiring a nightly for tray integration.
Exact dependency versions and native build requirements still need pinning and
validation. Wails bundles application web assets rather than a Node server.

[Bubble Tea](https://github.com/charmbracelet/bubbletea) supplies the Go TUI
frontend. Neither frontend should own a separate rule engine.

[getlantern/systray](https://github.com/getlantern/systray) is a smaller tray-only
alternative. It would still require choosing and integrating a window toolkit
or introducing a browser-based local control server. Wails combines the window
and tray needs; this is a proposed tradeoff, not a measured footprint comparison.

## Windows native power request

Microsoft documents handle-based [PowerCreateRequest](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-powercreaterequest),
[PowerSetRequest](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-powersetrequest)
and [PowerClearRequest](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-powerclearrequest).
SystemRequired addresses idle sleep without requesting the display remain on.
The handle-based API fits a Go engine without SetThreadExecutionState's thread
affinity. Windows may terminate requests for explicit sleep; Microsoft also
documents a Modern Standby battery restriction. These are support limits to
present, not defects to hide with simulated input or global policy changes.
No native power call was executed for this design work.

## T3 and WSL

A read-only call in this session to `t3_thread_list` with all projects, child
agents, the five proposed busy statuses and limit 1 succeeded. Its aggregate
reported total 5 and a non-null next cursor. No unrelated thread contents were
read. This confirms the current tool accepts these inputs; it does not prove
that an external Windows application can authenticate or query the same server.

The installed local server reports T3 version `0.1.7-nightly.20261101.4016` and
loopback origin on port 3773. T3's [external-agent documentation](https://github.com/pingdotgg/t3code/blob/main/docs/user/outside-agents.md)
describes OAuth/read-only access, local MCP URLs and eventual sign-in expiry.
That expiry must surface as an observation failure. The current agent's injected
credentials are not a Dozolt credential source.

The [official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) is a
candidate protocol client. Its compatibility table includes qualifications for
client OAuth support, so a selected version needs a real T3 login test. Do not
assume an SDK label establishes end-to-end compatibility.

[Microsoft WSL networking guidance](https://learn.microsoft.com/en-us/windows/wsl/networking)
documents Windows access to WSL-hosted services through localhost, including the
default NAT case. The actual installed T3 route still needs verification from a
Windows process. No network or WSL settings were changed.

## Mac lid closure

[Apple clamshell guidance](https://support.apple.com/en-us/102501) documents the
external-display/power/accessory setup. It does not establish continued travel
work with no external display.

Apple's own [pmset source](https://github.com/apple-oss-distributions/PowerManagement/blob/main/pmset/pmset.m)
parses `disablesleep` into the system-wide `SleepDisabled` setting. This provides
mechanism evidence, not a stable public API guarantee or hardware test.
[Amphetamine Power Protect](https://github.com/x74353/Amphetamine-Power-Protect)
also documents difficulties around power-source changes. An optional privileged
helper with bounded ownership and recovery is therefore a candidate requiring
verification. No Mac hardware was accessed and no power setting was changed.

## Linux and wake networking

[systemd-logind's D-Bus contract](https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.login1.html)
documents inhibition and permissions. Desktop policy and tray support still
need a declared support matrix and physical integration checks.

[Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve) documents
identity headers, localhost-only backend guidance and missing user headers for
tagged clients. Use it only for wake handlers. Tailscale's
[wake-host example](https://tailscale.com/blog/wake-on-lan-tailscale-upsnap)
supports the awake-LAN-host topology. Neither document verifies the user's NIC,
firmware, sleep state or the media PC's uptime.

[tsnet](https://tailscale.com/docs/features/tsnet) is an alternative embedded
network stack, but it adds another node identity and login lifecycle. Reusing the
installed Tailscale with Serve is the simpler proposed integration. Its setup
must preserve existing services. No Serve configuration was applied.

## State and authority

The owner posted the awd-config hotfix memo and authorized provisional drafting.
The installed hotfix now matches its complete receipt at source commit
`5a3e29a6ab2ead00ac572a7176e4500552184efe` (69 managed files verified). The retained
requirements review completed clean durable closeout on the exact original bytes;
the normal attestation-closure reader accepted it. The hourly monitor was disabled.
The same design run has resumed HLD authoring. No architecture approval,
product code, commit or publication is implied.

## Reconciliation after the hotfix

The privileged Mac helper was proposed in the draft. In architecture question 1,
the owner answered: "lets leave it as a maybe and investigate once we are actually
on a mac". The mechanism decision is therefore deferred until real-Mac
investigation; the helper is not selected or approved. Closed-lid work remains
required. Hardware and recovery evidence remain absent. This is a deferral, not
a truth fix for closed-lid work or permission to substitute an external-display-
only outcome. Windows design can continue while the whole plan retains the
dependent Mac investigation, decision and verification work.

The [Serve CLI reference](https://tailscale.com/docs/reference/tailscale-cli/serve)
documents selectable HTTPS ports. The proposal uses a dedicated port and a
user-executed command rather than adding a Dozolt controller for Serve state,
conflict reconciliation and rollback. Existing Serve configuration is inspected
but not rewritten by Dozolt. No Tailscale settings were changed during research.

## Owner clarification of manual controls

The owner requested easily accessible unconditional Stay awake controls with an
optional time limit that reverts afterward. The HLD uses the existing manual
mode and native hold controller, with direct tray/window/TUI actions and a
visible countdown. The timer returns to Auto, not immediate Sleep. The stated
interpretation of unconditional is independent of probe/rule state; configured
battery limits and OS restrictions still apply. This clarifies the existing
control requirement and does not add another prevention mechanism.

For architecture question 2, the owner replied "ok" to the recommended startup
default. The coordinator stated its interpretation: wait for confirmed activity,
configurable per probe. The HLD now makes both choices explicit. This response
does not supply whole-HLD approval. The two-question allowance is consumed,
including the express deferral of the Mac mechanism.
