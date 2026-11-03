# WSL process-check lifecycle evidence and owner decision

This is supporting evidence for the current architecture and implementation
plan. It does not authorize product implementation, commits or publication.

The original plan promised that process inspection would never start a stopped
WSL distribution. Its proposed method checked running state, then launched a
fixed command through `wsl.exe`. Review finding R1-1 identified the gap between
those two operations.

## Native experiment

On 2026-11-03, the owner approved an isolated lifecycle experiment. It used one
new UUID-named temporary WSL2 distribution on Windows 10.0.26300.9818 with WSL
3.0.3.0 and kernel 6.18.54.1-1. No existing distribution was targeted for launch,
termination, configuration or removal.

The experiment observed the fixture running, terminated it, confirmed it was
absent from the running list, and executed the previously admitted inspection
command. The command ran successfully and the distribution appeared in the
running list again. This demonstrates the check-then-launch restart race.

The fixture was then terminated and unregistered. A subsequent native list
matched the four original registrations, and the fixture directory was absent.
The runner's final directory-removal check reported a missing directory because
WSL unregister had already removed it. The original result and separate cleanup
reconciliation remain in the owning review trail; no repeated native experiment
was needed to reconcile that error.

This was a disposable lifecycle probe. It did not run product code, test actual
sleep prevention, perform external T3 OAuth, send WOL packets or establish
complete Windows acceptance.

## Alternatives considered

The installed CLI's `--running` option belongs to listing, not atomic conditional
execution. The documented `WslLaunch` interface has no open-existing-only option.
Source inspection found an internal `OPEN_EXISTING` COM flag, but that private
interface was not selected, tested or approved as a product dependency. A
resident WSL observer would add installation and lifecycle work. Neither extra
mechanism is needed for the accepted policy below.

Sources: [WSL commands](https://learn.microsoft.com/en-us/windows/wsl/basic-commands),
[WslLaunch](https://learn.microsoft.com/en-us/windows/win32/api/wslapi/nf-wslapi-wsllaunch),
and the [official service implementation](https://github.com/microsoft/WSL/blob/master/src/windows/service/exe/LxssUserSession.cpp).
Inspected source bytes and native outputs are retained in the review trail;
moving documentation is context, not a substitute for product acceptance.

## Accepted policy

The coordinator proposed: “skip checks when WSL is already stopped; if the rare
race restarts it, leave it running. Never automatically shut it down.” The owner
replied: “this is acceptable. I think its a pretty unlikely scenario”.

Apply that policy to the explicitly selected distribution's generic application
process checks. Check its state before each inspection command. Stopped,
missing or unreadable state means no inspection launch and an unknown result.
A shutdown after that check can still cause a restart. Explain that limitation
in WSL application setup/settings and never automatically terminate a
distribution afterward. T3 polling uses its existing authorized local endpoint.

This is an owner-approved behavior and representation change. It does not make
the former absolute no-start claim true. The design must state the accepted
best-effort behavior and product tests must verify it. No extra helper, private
API dependency or deliberate launch after an observed stopped state is approved.
Requirements, platform order, the full eighteen-slice goal, Mac mechanism
deferral and other accepted decisions remain unchanged. Affected exact-byte
architecture/plan review is required; final slice and delivery authority remain
separate gates.
