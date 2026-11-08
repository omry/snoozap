# Continuous integration

Every push and pull request runs `.github/workflows/ci.yml` without path filters.
The stable check names are `CI / Windows`, `CI / macOS` and `CI / Linux`.
Each job has a 20-minute limit. A failing job does not cancel the other platforms.

| Check | Hosted runner | Native Go target | Race tests |
| --- | --- | --- | --- |
| CI / Windows | `windows-2025` | windows/amd64 | Yes, with the hosted GCC toolchain |
| CI / macOS | `macos-15` | darwin/arm64 | Yes, with the hosted compiler |
| CI / Linux | `ubuntu-24.04` | linux/amd64 | Yes, with the hosted GCC toolchain |

Go comes from the exact `go` directive in `go.mod`. The job verifies that version
and sets `GOTOOLCHAIN=local` to prevent an automatic toolchain upgrade.
External actions use full commit pins. The token has only `contents: read`;
checkout does not persist credentials. CI uses no repository secrets or release
publishing, and does not change repository protection settings.

Run these commands from the repository root with the pinned Go version, Bash,
Git, and a C compiler available. On Windows use Git Bash and a GCC toolchain.
Use LF checkouts; CI disables automatic CRLF conversion before checkout.

```bash
set -euo pipefail
export GOTOOLCHAIN=local GOFLAGS=-mod=readonly CGO_ENABLED=1
export SNOOZAP_NATIVE_ACCEPTANCE=0
test "$(go env GOVERSION)" = "go$(awk '$1 == "go" {print $2}' go.mod)"
unformatted=$(git ls-files -z -- '*.go' | xargs -0 gofmt -l)
test -z "$unformatted" || { printf '%s\n' "$unformatted"; exit 1; }
go mod verify
go vet ./...
go test -count=1 -timeout=5m ./...
go test -race -count=1 -timeout=5m ./...
go build ./...
git diff --exit-code -- go.mod go.sum
```

For the Windows executable checks, set `SNOOZAP_CI_OUTPUT` to an existing
temporary output directory (the workflow uses its supplied `RUNNER_TEMP`):

```bash
go build -tags=production -o "$SNOOZAP_CI_OUTPUT/snoozap.exe" ./cmd/snoozap
go build -tags=production -ldflags='-H windowsgui' -o "$SNOOZAP_CI_OUTPUT/snoozap-tray.exe" ./cmd/snoozap-tray
```

Each workflow command must succeed. Formatting differences produce a nonzero
exit; Go and module failures fail their step. Dependency commands use read-only
module mode, and a final check rejects changes to `go.mod` or `go.sum`.
GitHub retains the individual command output in each platform's job log.

These jobs test only packages selected by the current platform build constraints.
The Windows entry points and native adapters do not exist in the macOS/Linux
builds yet. Green jobs do not qualify either product port or any physical device.
Opt-in native acceptance stays disabled: GUI/tray interaction, real power
requests, WSL lifecycle, live T3 authorization, credentials, sleep/lid/battery,
network and wake hardware journeys require their separate acceptance records.
Ordinary unit tests that skip a native fixture do not count as native coverage.
