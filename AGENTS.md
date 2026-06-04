# Orientation for AI agents

If you're an AI coding agent (Claude Code, opencode, Codex, etc.) about to work in this repo, start here.

## What this project is

`resleeve` is a self-hosted memory + session-state layer for AI coding agents. See the [README](README.md) for the pitch and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the technical overview.

## Where to look first

- **Make a change**: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — package layout, data flow, the daemon/serve split, key tables, hook + MCP contracts.
- **Understand the verbs**: [docs/USER_GUIDE.md](docs/USER_GUIDE.md) — what each `resleeve` subcommand does end-to-end.
- **Run smoke tests before committing**: [docs/SMOKE.md](docs/SMOKE.md) — the non-destructive checklist.
- **Cut a release**: [docs/RELEASING.md](docs/RELEASING.md).
- **Design context** for non-obvious decisions: `docs/design/round-{1,2,3,4}/*.md`. Each round's `00-*-decisions.md` documents the locked tradeoffs.

## House rules

- **Tests must stay green.** `go vet ./...` and `go test ./...` both pass on every commit. There is no "I'll fix the test later" branch.
- **Don't add unused code, vendor dependencies, or feature flags speculatively.** If you're not sure something will be needed, leave it out.
- **Match the existing patterns.** New verbs use `flag.FlagSet`. New 4xx responses use the structured error envelope (`internal/serve/errors.go` + `internal/agent/errors_envelope.go`). New TEXT timestamp columns that are lex-compared use the fixed-width nanosecond layout (see `internal/storage/sql/sqlite/sync.go`).
- **No co-author trailers on commits.** Plain commits authored by the human; do not add `Co-Authored-By:` lines for AI tools.
- **No `--no-verify`, no `git config` mutations, no `--amend` on pushed commits, no force-push to main without explicit approval.**

## Memory and scope

This repo has a `.resleeve-scope` marker at the root pinning the scope `resleeve`. If you have a `resleeve` daemon running, the `SessionStart` hook will inject the rolled-up plan + learnings into your context automatically. See [docs/USER_GUIDE.md](docs/USER_GUIDE.md) for memory curation via CLI or MCP.

## When you're done

If you've made a change worth a future agent knowing about (a new pattern, a hidden constraint, a recovered bug), append a learning:

```bash
./resleeve learning append resleeve "<one-paragraph lesson worth carrying forward>"
```

Don't auto-curate; only write learnings when there's a concrete takeaway.
