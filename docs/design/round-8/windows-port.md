# Round 8 — Windows port

> **Status**: planning. Tracks under sub-scope `resleeve/windows-port` in the memory store. Not yet executed.

## Goal

Make resleeve a first-class Windows citizen. Today macOS and Linux are CI-tested every PR; `windows-latest` is dropped from the test matrix because `internal/cli/lifecycle.go` fails to compile on Windows. The Windows port closes that gap.

## What's blocking compile

`go build ./... GOOS=windows` fails with two POSIX-only symbols in `internal/cli/lifecycle.go`:

- **line 102** — `syscall.Kill(pid, syscall.SIGTERM)` — `syscall.Kill` is unix-only.
- **line 649** — `syscall.SysProcAttr{Setsid: true}` — the `Setsid` field does not exist on Windows.

Both sit in helpers that manage the daemon's process group (graceful shutdown signal; detached spawn from the parent's controlling terminal).

## Fix shape

Split `internal/cli/lifecycle.go` into three files via build tags:

| file | build tag | contents |
|---|---|---|
| `lifecycle.go` | (no tag, all platforms) | `runUp`, `runDown` body, `runDoctor`, `runPurge`, `runUsage`, all the `print*` helpers, anything platform-portable. |
| `lifecycle_unix.go` | `//go:build unix` | `terminateDaemon(pid int) error` wrapping `syscall.Kill(pid, syscall.SIGTERM)`. `daemonSysProcAttr() *syscall.SysProcAttr` returning `&syscall.SysProcAttr{Setsid: true}`. |
| `lifecycle_windows.go` | `//go:build windows` | `terminateDaemon(pid int) error` using `os.Process.Kill()` — no graceful SIGTERM equivalent that's stdlib-clean for our use case; daemon cleanup happens in its own context-cancel path which won't run on hard-kill, but is tolerable for v1. `daemonSysProcAttr() *syscall.SysProcAttr` returning `&syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}` to detach the spawned daemon from the parent's console. |

Update the call sites in `lifecycle.go` to use the platform-extracted helpers.

## Other surface to audit

### Filesystem and path handling

- `internal/adapter/claude/install.go` — `~/.claude/settings.json` path. Today uses `os.UserHomeDir()` + `filepath.Join("...")`. `UserHomeDir` returns `%USERPROFILE%` on Windows. Should work as-is; verify with `resleeve install-bridge --dry-run` on Windows. Expected path: `C:\Users\<name>\.claude\settings.json`.
- `internal/agent/endpoint.go` — `$XDG_RUNTIME_DIR` doesn't exist on Windows. The code already has a fallback to `$HOME/.resleeve/endpoint`; confirm that branch fires on Windows.
- PID file under data dir — should be fine, Go's `os` package handles per-platform paths.
- Reconcile sweep over `~/.claude/projects/**/*.jsonl` — uses `filepath.Glob`; cross-platform by construction.

### Keychain

`zalando/go-keyring` (the dep we pulled in for followup A) picks the `wincred` backend on Windows. The dep is already in `go.mod`; no new deps required.

### Test files

Any `_test.go` that:

- imports `syscall` and uses POSIX-only symbols,
- sends Unix signals,
- forks via `os.StartProcess` with `SysProcAttr` set,

needs a `//go:build unix` guard or a Windows-specific sibling. Audit before flipping the matrix on.

### Specific tests likely affected

- `internal/agent/daemon_test.go` — if it spawns the daemon via `os.StartProcess`, may need Windows variant.
- `internal/cli/lifecycle_*_test.go` (if/when added) — should be tagged matching the corresponding source file.

## Tests to add as part of the port

- `lifecycle_windows_test.go` (`//go:build windows`) — spawn a dummy process, call `terminateDaemon` on its PID, assert `process.Wait` reports the exit. Verifies the Windows path actually terminates rather than no-op'ing.
- `lifecycle_unix_test.go` (`//go:build unix`) — same shape against `terminateDaemon` on Linux/macOS, asserts SIGTERM was delivered (via a child that traps SIGTERM and exits with a marker code).

## CI matrix

After compile passes on Windows:

```yaml
# .github/workflows/ci.yml
matrix:
  os: [ubuntu-latest, macos-latest, windows-latest]
```

Drop the temporary comment about the port-in-flight at the same time.

## Acceptance criteria

- `go vet ./...` clean on `windows-latest` in CI.
- `go test ./...` green on `windows-latest`.
- `.github/workflows/ci.yml` matrix re-includes `windows-latest` and the temp comment is removed.
- Smoke on a real Windows box (or in CI runner): `resleeve up`, `resleeve doctor`, `resleeve down` all work from a clean install. Hook injection visible in a fresh `claude` session.

## Out of scope for round 8

Land separately if/when there's demand:

- Windows installer packaging (Chocolatey, Scoop, winget).
- Run-as-Windows-Service (NSSM, `sc.exe create`, or built-in service support).
- Code signing (Authenticode) for the released binary.
- Long Path support (`\\?\` prefix) for paths > 260 chars in deep workspace trees.

## How to execute

The brief above is enough for whoever picks this up. Two reasonable approaches:

1. **Local development on a Windows box.** Clone the repo, do the build-tag split, run `go test ./...`, iterate until green. Open a PR from the Windows machine.
2. **Cross-compile from Mac/Linux + remote validation.** `GOOS=windows go build ./...` will tell you the compile errors quickly; the split can be drafted from any host. Push to a branch and let CI run on `windows-latest` as the smoke.

Either way, this doc is the contract. Match it; deviate only with reason.
