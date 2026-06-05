# Round 8 — Windows port

> **Status**: executed. Tracks under sub-scope `resleeve/windows-port` in the memory store. See "Delta from brief" at the bottom for where the implementation corrected this doc.

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

## Delta from brief (recorded at execution)

Reading the code + cross-compiling (`GOOS=windows`) + inspecting Claude Code's own bundle turned up
gaps and one mis-diagnosis in the brief above. What actually shipped:

- **Only two compile blockers**, both in `internal/cli/lifecycle.go` (`syscall.Kill`,
  `SysProcAttr.Setsid`) — confirmed by `GOOS=windows go build ./...`.
- **`syscall.Exec` is NOT a compile blocker.** It is declared on Windows (returns `EWINDOWS` at
  runtime), so `internal/cli/resume.go` cross-compiles clean. It's a *functional* fix: extracted to
  `execResume` (`resume_unix.go` exec-replaces; `resume_windows.go` runs the child then `os.Exit`s
  with its code).
- **`daemonAlive` liveness was unlisted.** `proc.Signal(syscall.Signal(0))` compiles on Windows but
  errors at runtime → `daemonAlive` always false → up/down/doctor wrong. Extracted to `processAlive`
  (Windows uses `OpenProcess` + `GetExitCodeProcess`, alive iff exit code == `STILL_ACTIVE`/259).
- **`syscall.STILL_ACTIVE` does not exist** in stdlib `syscall` or `golang.org/x/sys/windows` —
  defined locally as `const stillActive = 259`.
- **`native_resume.go` `sh -c` was unlisted** — no `sh` on bare Windows. Prime-mode command is now
  built per platform (`primeResumeCmd`): `sh -c` on unix, `cmd /c` on Windows (both `<`-redirect the
  prompt file; Windows quoting avoids Go `%q` backslash-escaping).
- **`encodeCwdForProjectDir` was wrong on every platform.** CC's actual encoder (from its bundle) is
  `s.replace(/[^a-zA-Z0-9]/g, "-")`; the old Go replaced only `/`, so replay silently wrote to the
  wrong dir for any cwd containing `.`/`_`/space. Fixed in place with the same `[^a-zA-Z0-9] -> "-"`
  rule (platform-agnostic — no build tag; backslash and drive-colon fall out for free).
- **No existing `_test.go` used POSIX syscalls** — none used `syscall`/signals/`StartProcess`, so none
  needed a build-tag guard for that reason. New tagged tests were added for the unix/Windows process
  primitives; the encode + prime-cmd tests are platform-aware in one untagged file.

## Delta from brief — round 2 (actually running `go test` on a Windows box)

Cross-compiling only proves the binary *builds*. Running the full suite on real Windows surfaced a
batch of **pre-existing, latent Windows breakage** that had been invisible while `windows-latest` was
out of the matrix. None were regressions; all are now fixed so the suite is green and CI can stay
green. Lesson: validate the matrix flip by running the tests on the OS, not just cross-compiling.

- **`os.UserHomeDir()` reads `%USERPROFILE%`, not `$HOME`, on Windows.** ~13 tests sandboxed the home
  dir with `t.Setenv("HOME", tmp)` only, so on Windows they silently hit the real home and failed.
  Added `internal/testutil.SetHomeDir(t, dir)` (sets both vars) and routed every call site through it.
- **`exec.LookPath` requires a PATHEXT extension on Windows.** The opencode detect test wrote a fake
  binary named `opencode` (no `.exe`) → never found. Test now appends `.exe` on Windows.
- **Unix permission bits aren't enforced on Windows.** The file-keychain test asserted mode `0600`
  (Windows reports `0666`); that assertion is now skipped on Windows only.
- **`:` (and `<>"\|?*`) are illegal in Windows filenames — a real code bug, not just a test issue.**
  `encodeScopePath` maps scope `/`→`:`, and `internal/sync/local` wrote those keys straight to disk,
  so any memory blob with a child scope segment failed to write on Windows (`os.Rename` →
  "The parameter is incorrect"). The local backend now percent-encodes reserved bytes per path
  segment (`encodeKey`/`decodeKey`, round-tripped in `List`), applied uniformly on all platforms so
  the on-disk layout is identical everywhere. (The primary `serve`/SQLite sync path keys by string
  and was unaffected.)

Validated on a real Windows 11 box: `go build`/`go vet`/`go test ./...` all green; `go vet ./...`
green cross-compiled for `linux` and `darwin`; and a sandboxed `resleeve up` → `doctor` (daemon ✓
running + bridge ✓) → `down` smoke passed — exercising the detached spawn, `processAlive`, and
`terminateDaemon` paths for real.
