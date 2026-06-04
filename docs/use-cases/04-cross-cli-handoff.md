# Use case 04 — Cross-CLI handoff

## The problem

You spent four hours in a Claude Code session walking a refactor through to a
working state. Now you want to keep going — but in a *fresh* CLI process:

- Same vendor, different terminal / machine / day. Laptop on the train,
  workstation at home; resume the same conversation tomorrow morning.
- Different vendor. opencode handles a chunk better, or your team standardized
  on a future codex build, or your Claude quota tapped out. The new CLI should
  start where the old one left off — same plan, same context, same pending
  question.

`resleeve resume` is the verb. It has two render modes.

## Two modes

| Mode | When | What lands on disk | Fidelity |
| --- | --- | --- | --- |
| **Replay** | source CLI == target CLI, adapter supports it | the target's native local format (for claude: a `.jsonl` under `~/.claude/projects/<cwd>/`) | full — bit-equivalent up to the resume point when `vendor.native_payload` was preserved |
| **Prime** | source CLI != target CLI, or replay isn't supported | a synthesized markdown opening prompt under `~/.resleeve/hydrate/<new-uuid>.md` | lossy — a single summary prompt, not a multi-turn history |

`--mode auto` (the default) picks replay when source and target match, prime
otherwise. opencode is forced into prime even under auto — its adapter can't
ingest claude JSONL.

## Walkthrough — same-CLI replay (claude → claude)

Pick a session:

```bash
resleeve session list --limit 5
# ses_a1b2c3   active   claude   resleeve/docs-uc-cross-cli   2026-06-04 14:02
# ses_9d8e7f   ended    claude   resleeve/round-4-impl        2026-06-03 22:11
# ...
```

Resume it:

```bash
resleeve resume ses_a1b2c3
# ✓ hydrated to /Users/you/.claude/projects/-Users-you-resleeve/ses_a1b2c3.jsonl (replay mode)
# (exec's into: claude --resume ses_a1b2c3)
```

Auto mode noticed source==target==`claude`, called the claude adapter's
replay path, wrote the JSONL to the project dir Claude Code expects, then
`syscall.Exec`'d `claude --resume <sid>`. The resleeve process is replaced;
you're inside Claude Code as if you'd run it yourself.

To inspect the command without exec'ing — useful in scripts and dotfiles:

```bash
resleeve resume ses_a1b2c3 --print
# claude --resume ses_a1b2c3
```

## Walkthrough — cross-CLI prime (claude → opencode)

```bash
resleeve resume ses_a1b2c3 --cli opencode
# ✓ hydrated to /Users/you/.resleeve/hydrate/9f1e...md (prime mode)
#   ⚠ prime mode: synthesized opening prompt; cross-CLI translation is lossy by design
# (exec's into: opencode --prompt-file /Users/you/.resleeve/hydrate/9f1e...md)
```

`--cli opencode` overrides the target. Since target (`opencode`) != source
(`claude`), auto-mode resolves to prime, and the opencode adapter renders the
opening prompt via `common.SynthesizePrime`. The prompt has four sections:

```markdown
# Resumed session: ses_a1b2c3

## Context
- Source CLI: claude 1.0.42
- Working directory: /Users/you/resleeve
- Git branch: docs/uc-cross-cli
- Model: claude-opus-4-7
- Started: 2026-06-04T14:02:11Z

## Plan
<rolled-up memory context for the session's scope — same body
 `resleeve context <scope>` and the SessionStart hook emit>

## Recent activity
- 2026-06-04T14:05:01Z user: walk me through the round-4 adapter split
- 2026-06-04T14:08:22Z called Read({"path":"internal/adapter/adapter.go"}) → 412 lines
- ... (last N=20, oldest dropped first if over byte cap)

## Next
Last user message — here's where we left off:
> draft the use-case doc now
```

The `## Plan` body is fetched by `runResume` itself, not the adapter:
`fetchPrimePlan` (in `internal/cli/resume.go`) hits `GET /v1/context` against
the session's scope and passes the resolved markdown into
`HydrateOpts.PlanContent`. The synthesizer is adapter-agnostic, so a future
codex adapter gets the same plan injection for free. Empty scopes or the
sentinel `"unknown"` render `(none captured)`; fetch failures are non-fatal.

Force prime on a same-vendor handoff when the captured session is huge and you
want a summary rather than a full replay:

```bash
resleeve resume ses_a1b2c3 --mode prime
```

## The `--cwd` override

The captured session pins to its original `cwd`. If you've moved the repo, or
you want to resume against a worktree elsewhere on the same machine, re-root
it:

```bash
resleeve resume ses_a1b2c3 --cwd ~/work/resleeve-worktree
```

For replay mode this changes the target project dir Claude Code looks under
(`~/.claude/projects/-Users-you-work-resleeve-worktree/`). For prime, it
overrides the `Working directory:` line in the synthesized `## Context`.

## On-demand pull integration

`resume` is a read-heavy verb. Before resolving the session id locally, it
fires a best-effort `POST /v1/sync/pull-now` against the daemon, capped at 2s.
A session captured on machine A ten seconds ago is resumable on machine B
without waiting for the 30s slow-tier polling tick.

Pull failures are non-fatal: `resume` warns to stderr and falls back to local
state. Skip the pull entirely for fast offline use:

```bash
resleeve resume ses_a1b2c3 --no-pull
```

## Caveats

- opencode is a v1 stub. `Hydrate` works (prime-only); capture-side verbs
  (`InstallBridge`, `FromNative`) still return `ErrNotImplemented`. You can
  re-sleeve claude → opencode, not yet the reverse.
- Replay-mode opencode errors at the adapter level — it can't ingest claude
  JSONL. `--cli opencode --mode replay` rejects cleanly.
- Replay fidelity depends on `vendor.native_payload`. Reconcile-synthesized
  events (e.g. a backfilled `session_start`) get a stderr note:
  `N event(s) without native payload were skipped`.
- After `syscall.Exec`, resleeve's process is replaced — no post-resume hook,
  no exit-code propagation. Use `--print` to wrap the launch yourself.

## See also

- [`../USER_GUIDE.md`](../USER_GUIDE.md) §6 — full flag reference for `resume`.
- [`../design/round-4/01-conversation-transport.md`](../design/round-4/01-conversation-transport.md) — the replay/prime split design.
- [`02-cross-machine-sync.md`](02-cross-machine-sync.md) — the on-demand-pull
  story this verb plugs into.
