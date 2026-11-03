> Historical pre-rename evidence for SnooZap (formerly Dozolt). Names, decisions
> and review/hash observations below describe that earlier state. See
> [rename provenance](../../supporting/rename-notes.md).

# Engineering evidence and estimate basis

Inspected on 2026-11-03. The repository contains design documents and review
scratch only. There is no product source, module, build pipeline, test harness,
installed Dozolt or previous release to reuse. This is a greenfield estimate.

The owner approved the reviewed architecture and selected UX with `yes` to:
“Do you also approve the reviewed architecture so I can prepare the implementation
plan?” This permits planning only. The accepted architecture preimage is SHA256
`e47184398fd8c0edb6b0142b6c4a387f8692311b93a8a099c069d795fa3834b5`.
Requirements preimage is SHA256
`7095e8834c48447242cdd60ef42239f3eec78660a9c42ad90c0fd914c223f982`.
Their prose status headers predate completed reviews/approval; the retained
approval and verified review closure are the current evidence. They were not
edited merely to change those headers and invalidate their exact-byte review.

## Inspected prerequisites

- [Wails v3.1.0 release](https://github.com/wailsapp/wails/releases/tag/v3.1.0)
  is a stable release, distinct from v3.2 nightlies. Its
  [module manifest](https://github.com/wailsapp/wails/blob/v3.1.0/v3/go.mod)
  declares Go 1.26 and toolchain go1.26.10. Use that desktop version initially.
- [Bubble Tea v2.1.1](https://github.com/charmbracelet/bubbletea/releases/tag/v2.1.1)
  and its [module manifest](https://github.com/charmbracelet/bubbletea/blob/v2.1.1/go.mod)
  use `charm.land/bubbletea/v2` and Go 1.26. Its independent major-version import
  can coexist with Wails tooling's v1 dependency; do not silently import v1 for
  the product TUI. Pin Go 1.26.10 as the initial shared toolchain.
- [Official MCP Go SDK v1.8.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0)
  supplies bounded transports and OAuth support. Its
  [manifest](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/go.mod)
  requires Go 1.25, compatible with the selected toolchain. End-to-end T3
  authentication is still an acceptance prerequisite, not a completed test.
- [T3 outside-agent documentation](https://github.com/pingdotgg/t3code/blob/main/docs/user/outside-agents.md)
  specifies HTTP MCP with OAuth, default read-only authorization, localhost
  callback and revocation/expiry. The earlier tool-schema check establishes the
  required list filters exist in the installed nightly, not that an external
  Windows client has logged in. Never reuse this chat's injected credentials.
- [Microsoft PowerSetRequest](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-powersetrequest)
  documents SystemRequired, user-initiated sleep termination, and Modern Standby
  on-DC termination five minutes after the system sleep timeout. The engine must
  expose those limits and must not periodically renew requests to evade policy.
- [WSL networking](https://learn.microsoft.com/en-us/windows/wsl/networking)
  supports Windows localhost access to WSL services. Distribution identity and
  native-versus-WSL port collisions still need the Windows fixture test.
- [Tailscale Serve CLI](https://tailscale.com/docs/reference/tailscale-cli/serve)
  supports selected HTTPS ports and read-only status inspection. Existing
  mappings are inspected; the user runs any suggested setup command.
- The [UX notes](../high-level-design/ux-design.md) and
  [simulation](../high-level-design/ux-prototype.html) are owner-selected context.
  They exercise interactions with synthetic observations, not native power,
  OAuth, packaging, persistence, network or accessibility evidence.

These are primary source inspections. No dependency was installed, no executable
was built, and no physical sleep, OAuth, Tailscale, Mac or WOL test ran here.
Do not carry moving documentation as an implicit update to pinned product code.

## Classification and decision boundary

Internal ownership, serialized policy, authenticated local IPC, process matching,
poll cancellation, bounded parsers and atomic configuration are ordinary
engineering choices specified in the plan. No new public feature or trust grant
is selected here. Unsupported platform/SDK behavior discovered during delivery
returns to the affected architecture decision before dependent implementation.

The only retained owner decision is Mac closed-lid mechanism selection. The
owner explicitly postponed it until work on real Mac hardware. The plan retains
the investigation, decision, dependent implementation and integrated verification.
No candidate helper, privilege grant or machine-wide setting is approved.
This is a deferral, not evidence that closed-lid operation works. Windows planning
can be assessed separately; the whole project cannot be called implementation
ready while that part is unresolved.

## Estimate basis

Ranges count incremental final delivered nonblank physical lines, excluding
comment-only lines. Docstrings and multiline statements count physically.
Production includes runtime code, bundled UI, shipped configuration, packaging
and user instructions. Tests include test-only helpers/configuration, protocol
fixtures generated by code and integration drivers. Development design documents,
static fixture data, images, generated bindings, lockfiles, dependencies and
disposable experiments do not count. No edit churn or time conversion is used.

Ranges are based on the concrete modules, interfaces and failure paths in the
plan and canonical slice descriptions. Shared engine/config/history/UI/wake code
is counted once in Windows prerequisites; subsequent ports count new adapters,
platform packaging and their new tests only. All original product code is absent,
so ranges are wider than an estimate against an existing implementation.

Mac closed-lid ranges reserve a conditional implementation envelope, not an
approved mechanism or a ready work item. If the real-Mac result requires a
different privilege, installation or public behavior contract, refresh affected
architecture review/approval and re-estimate that work before admission. Do not
force a result into this envelope. The design-role investigation delivers evidence,
so its development documents and disposable probes have zero counted code lines.
