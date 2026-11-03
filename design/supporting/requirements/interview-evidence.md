> Historical pre-rename evidence for SnooZap (formerly Dozolt). Names, decisions
> and review/hash observations below describe that earlier state. See
> [rename provenance](../../supporting/rename-notes.md).

# Context before the design interview

This is retained discussion context, not an approved requirements document.

The project began after Windows logs showed idle sleep during an unattended period. T3 exposes running/waiting status through its MCP t3_thread_list operation; a live check succeeded on 2026-11-02. A portable app was proposed to monitor that status and hold a host OS sleep request. This was extended by the owner to generic app predicates and AND/OR/NOT composition, with a simple tray UI, diagnostics and API history, controls and a TUI. Go was discussed as a candidate.

The owner selected Windows-first vertical delivery followed by Mac and Linux. Mac closed-lid operation is required; ordinary idle inhibitors are insufficient evidence of that behavior. Remote wake is a bonus: an always-on LAN helper could receive an authorized request through Tailscale and send Wake-on-LAN. A Linux media PC exists but its uptime is unknown. An R7000 currently has stock firmware; flashing it is an alternative, not authorized execution. Existing tools did not establish a complete cross-platform T3-aware fit.

The name Dozolt was selected after web/GitHub software-name checks found no apparent collision. PyPI availability is irrelevant. The public repository https://github.com/omry/dozolt was created and verified empty. The owner now requests design here using AWD swe:design and existing context; product implementation remains excluded.

Exact design outputs: design/requirements.md, design/high-level-design.md, design/implementation-plan.md, design/overview.md and design/slices.json. Supporting artifacts belong to document-owned design/supporting/ subdirectories only when needed. The checkout is a new empty clone in the active scratch workspace; no source files or repository instructions were present.

## Remote-wake clarification

The owner proposed automatic trust between Dozolt instances on the same LAN. Initial advice overstated the benefit of pairing for local wake. Corrected analysis: hardware WOL recognizes a magic packet; Dozolt pairing only protects requests to a helper, not direct packets sent by another LAN device. The owner requested reevaluation. A proposed wake-only local cooperation mode is not yet approved. Remote requests need independent authorization; local discovery must not grant access to probes, diagnostics, credentials or settings.

Sources: Intel remote wake basics https://www.intel.com/content/www/us/en/support/articles/000005793.html ; Microsoft WOL methods https://learn.microsoft.com/en-us/windows-hardware/drivers/network/wol-methods-in-ndis-6-20 ; Tailscale grants https://tailscale.com/docs/features/access-control/grants . Tailscale documents external SSH/etherwake or UpSnap setups at https://tailscale.com/blog/wake-on-lan-tailscale-upsnap . Local Linux Tailscale 1.104.4 help did not expose a public wake command; upstream proposed tailscale wol PR https://github.com/tailscale/tailscale/pull/22085 was open and unmerged when checked.

## Accepted interview decisions

### Question 1: Unknown activity status

Owner answer: I think this should be configurable, because I'm not sure that there is a single answer here.

Recorded decision: Monitoring-failure behavior is configurable; no single fixed policy or default approved.

### Question 2: Rule setup and extension boundary

Owner answer: http probe is already opening up for user defined logic. approved for now.

Recorded decision: Approve the configurable built-in probe approach, including HTTP/API user-defined conditions; no command/script probes in current agreed scope.

### Question 3: Battery and closed-lid operation

Owner answer: yes

Recorded decision: Support battery operation with configurable battery limits, including technically supported Mac closed-lid operation.

### Question 4: Optional remote wake

Owner answer: seems ok for now

Recorded decision: Accept optional wake-only Dozolt cooperation: an enabled awake instance hosts a web interface listing current and previously discovered LAN Dozolts; Wake sends WOL using remembered adapter addresses. Local discovery and wake trust require no pairing. Remote requests use existing Tailscale authorization. Remote diagnostics/configuration and automatic T3 connection interception are excluded.

The owner also clarified monitoring-failure policy as "configurable per probe. with a defensive default". The exact interpretation of defensive has not been confirmed. The current requirements explicitly propose retaining sleep prevention after a previously active probe fails, for owner approval. Startup unknown state and logical combinations still require a concrete architecture contract.
