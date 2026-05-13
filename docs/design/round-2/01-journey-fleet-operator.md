# Journey: fleet operator (primary persona)

This is the journey that drives resleeve's design. Other personas — single-machine, cross-machine, cross-CLI, OSS-share — are derivative or degenerate cases of this one.

## Persona

One human user. Compute is elastic — k8s pods, VMs, Docker, laptop, cloud, doesn't matter. The user fans out N AI agent tasks across N ephemeral sleeves via an orchestrator (named target: **SCION** = `GoogleCloudPlatform/scion`). Sleeves are fungible — born for a task, killed when done. The **stack** (session content; in the long term, also memory) must be the persistent thing.

> **v1 scope note:** sessions only. Memory references in this journey (plans/learnings via the eventual `memory/` module) are roadmap, not v1. See [`../round-1/05-decisions.md`](../round-1/05-decisions.md) §"Relationship to ori."

## Setup (one-time)

1. User runs `resleeve serve` on a durable host (small VPS, k8s namespace, or laptop for dev). Master password → Argon2id-derived encryption key.
2. User registers resleeve with SCION Hub:
   ```
   resleeve orchestrator register scion --hub-url=https://hub.example.com
   ```
   Federated trust handshake — Hub learns to mint Agent Tokens that resleeve validates; resleeve learns the Hub's identity.
3. User adds a resleeve-sidecar block to the SCION agent template. Reference templates ship per harness; roughly:
   ```yaml
   agent:
     sidecars:
       - image: ghcr.io/mattkorwel/resleeve-sidecar:latest
         env:
           - RESLEEVE_SERVER: https://resleeve.example.com
           - RESLEEVE_GROVE: ${SCION_GROVE}
           - RESLEEVE_AGENT_TOKEN: ${SCION_AGENT_TOKEN}
   ```

## Golden path: fan out + complete

User: `scion run --template=fix-bugs --grove=auth-rewrite --parallel=5`

For each of the 5 agent pods:

1. **Pod boots.** SCION delivers: workspace mount (git worktree), Agent Token, env (`HARNESS=claude`, `SCION_GROVE=auth-rewrite`, `SCION_AGENT_NAME=claude-3`, ...).
2. **Sidecar starts.** Validates Agent Token with resleeve server. Resolves identity = `(grove=auth-rewrite, agent=claude-3)`.
3. **Sidecar hydrates.** Queries resleeve: *"is there a prior session for this slot?"*
   - If yes: fetch session blob; write the harness's native session file into the workspace (e.g., `~/.claude/projects/<sanitized>/<uuid>.jsonl`).
   - If no: fresh slot, skip.
4. **Sidecar installs bridge plugin** in the harness's plugin dir:
   - Claude: writes hooks to `~/.claude/settings.json` (`SessionStart`, `PostToolUse`, `Stop`).
   - Gemini: writes hooks to `~/.gemini/settings.json` (rich set: `SessionStart`, `SessionEnd`, `BeforeAgent`, `AfterAgent`, `BeforeTool`, `AfterTool`).
   - OpenCode: installs as an OpenCode plugin (no hooks; plugin notifies orchestrator).
5. **Harness boots in main container.** Picks up hydrated session via native resume (`claude --resume <id>`, `gemini /chat resume <tag>`, etc.).
6. **Agent runs.** Every assistant turn, tool call, tool result is shipped via bridge → sidecar (localhost) → resleeve server in real time.
7. **Agent completes or fails.** Bridge emits `session_end` with exit status. Sidecar flushes buffered events; marks session ended.
8. **Pod terminates.** Resleeve has the full transcript. *(Roadmap: plans/learnings via the `memory/` module also persist; v1 ships sessions only.)*

Net effect for the user:

- `resleeve fleet status --grove=auth-rewrite` shows 5 sessions (live → ended).
- `resleeve session search "auth migration"` finds the run that solved the bug.
- A new pod spawned for the same `(grove, agent_name)` picks up cleanly from where the last one ended.

## Sad paths

### S1. Pod dies mid-task (network blip, OOM, SCION restart)

**Default behavior:** sidecar buffers events not yet ack'd by upstream. Hard pod kill = buffer lost unless persisted.

**Design:** sidecar writes incoming events to a pod-lifetime local volume (k8s `emptyDir`) AND streams upstream. On SIGTERM, drain buffer within grace period. On hard kill, the local volume is lost — but the harness's native session file (on the workspace volume, typically the SCION-managed git worktree) survives. A reconcile pass on next pod boot for the same slot joins partial upstream events with the worktree's surviving session file.

**UX:** session shows `partial` status until reconcile completes.

### S2. Resleeve server unreachable

- Sidecar queues events to local volume; retries with exponential backoff.
- Bridge plugin never blocks on resleeve writes — keeps streaming to sidecar at localhost.

**UX:** agent finishes fine. Session appears in fleet status when connectivity returns.

### S3. Cross-pod resume on the same slot

A pod died with partial work. Orchestrator spawns a replacement for the same `(grove, agent_name)`.

- New sidecar hydrates from latest session blob (plus the worktree, which SCION persists).
- Harness loads via native resume — full fidelity if same vendor/version (encrypted thinking signatures, reasoning items preserved).
- Schema skew between pods: per-event `version` tag captured at write time; adapter routes by version.

### S4. Cross-harness re-sleeve

User explicitly: `scion run --template=continue --from-session=<id> --harness=opencode`.

- New pod boots with opencode instead of claude.
- Sidecar hydrates: `resleeve hydrate --session=<id> --target=opencode > /workspace/session.json`.
- Vendor-opaque fields dropped (Claude `thinking.signature`, Codex reasoning items). One-line warning recorded on session: *"extended thinking continuity not preserved across harness boundary."*
- Opencode loads the translated file. Message-text fidelity preserved.

### S5. Lost master password (standalone mode only)

Atuin's known UX pain point. Three options to decide in round 2:

- (a) **Recovery key issued at signup**, user must save offline. *Strong lean.*
- (b) Multi-device key wrap; lose one device but not all.
- (c) No recovery; forgotten password = lost data.

Under SCION mode this doesn't apply — keys are orchestrator-managed.

### S6. Orchestrator-delivered token expires mid-run

- Sidecar detects 401 from resleeve. Requests fresh Agent Token from SCION's credential-refresh path.
- Bridge keeps streaming to sidecar; sidecar handles upstream re-auth transparently. Agent unaware.

## Out-of-band UX (the human surface)

- `resleeve fleet status [--grove=...]` — live view of sessions per grove (live / idle / ended).
- `resleeve session show <id>` — replay event stream.
- `resleeve session search <query>` — full-text across decrypted history.
- `resleeve session share <id> --ttl=24h --redact=secrets,paths` — generate public link.
- `resleeve session export <id> --format=otel-genai` — pipe to observability tools.
- `resleeve memory plan write --scope=auth-rewrite` — *roadmap (v2+)*: feed the next fleet run via the memory module.

## Open design questions surfaced by this journey

1. **Sidecar buffering durability.** Pod-lifetime `emptyDir` covers graceful shutdown. For hard kill, lean on worktree-resident session files as the cross-pod truth, not the local volume.
2. **Event dedup on multi-source reconcile.** Per-event UUID + content hash. Sidecar AND file-watcher safety net may both deliver the same event; dedup by `(session_id, event_uuid)`.
3. **What "same agent slot" means across pods.** Adopt SCION's model: `(grove, agent_name)` is stable. Resleeve key = the same tuple.
4. **Standalone-mode parity.** Does laptop-mode go through the sidecar pattern (loopback) or use a built-in bridge plugin only? Lean: same bridge-plugin contract; "sidecar" becomes "local resleeve daemon" when running standalone.
5. **Lifecycle webhook to SCION Hub.** Emit `session_completed` back to SCION so the orchestrator can decide what to do next (spawn follow-up, mark grove done, etc.). Design API in round 2.
6. **Memory/session interplay during a run.** *(v2+.)* When the agent writes to `memory/`, stream immediately or batch at session end? Lean: stream immediately — memory writes are low-frequency anyway.
7. **Concurrent writers per scope.** *(v2+, with memory module.)* Multiple pods on the same grove. Sessions are per-pod, no conflict. Memory writes to the same scope from different pods: needs a conflict model (git push? last-writer-wins? CRDT?).
8. **SCION Hub trust setup direction.** Does resleeve issue tokens that SCION uses? Or does SCION mint tokens that resleeve validates? Both work; pick one in round 2.
9. **Sidecar-image distribution.** Where does `ghcr.io/mattkorwel/resleeve-sidecar` live? GHCR is the obvious answer; user namespace vs. organization namespace TBD.
10. **What to do when no orchestrator is involved at all.** A user runs `claude` directly on their laptop. Bridge plugin auto-detects: SCION env? → sidecar mode. Else? → local resleeve daemon mode. Same contract, different host.
