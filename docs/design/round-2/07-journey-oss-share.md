# Journey #4: OSS contributor sharing a session

A developer working in the open. They hit a bug, used Claude Code to debug it, and now want to share the relevant session — pinned in a GitHub issue, attached to a Slack thread, embedded in a blog post.

This is more **feature walkthrough than persona.** It tests the share subsystem: redaction rules, share-link UX, the recipient's view, access control, lifecycle.

## The use case

> "I solved this auth bug; here's the session where Claude figured it out. Pinned in the GitHub issue."

User invokes:

```
$ resleeve session list --recent
  01HZ001  auth-rewrite  2h ago  ended  47 events

$ resleeve session share 01HZ001 \
    --redact=secrets,absolute-paths \
    --ttl=7d \
    --public
  ✓ Share created: https://resleeve.example.com/s/Xyz3ABc
  ✓ Expires: 2026-05-19T22:00Z
  ✓ Redactions applied:
      - 14 absolute paths normalized to ~/.../
      - 2 strings matching secret patterns replaced
```

User pastes the URL into the issue / channel / blog.

## What the recipient sees

`https://resleeve.example.com/s/Xyz3ABc` →

A read-only web view:

- **Header:** session metadata (CLI + version, model, duration, scope, share creator's pseudonym/handle, expiration).
- **Transcript:** event-by-event, chronological. Tool calls/results rendered as collapsible blocks. Thinking blocks collapsed by default. Attachments inline if small + not redacted; otherwise `[redacted attachment, X bytes]`.
- **Side panel:** filter by event kind (hide thinking, hide system messages, etc.).
- **"Fork into your resleeve" button:** for recipients who have resleeve installed (see below).

No mutation. No comments. No login required for `--public` shares.

Format alternatives (v1 + v2):

- HTML page served from the resleeve server (**v1 default**).
- Markdown export (`?format=md`) for pasting into GitHub issues / Notion / etc. (**v1**).
- Static archive download (`?format=tar.gz`) for offline browsing (**v1**).
- Replayable / interactive ("asciinema for AI sessions") with original-timing playback (**v2+**).

## Redaction system

Resleeve has a redaction engine that runs on share-link generation (not on capture).

### Built-in rules

| Rule | What it does |
|---|---|
| `secrets` | Regex scan for high-confidence secret patterns (API keys, JWTs, AWS / GCP creds, GitHub tokens, OpenAI / Anthropic / Google keys). Replaces with `[REDACTED-SECRET-N]`. |
| `env` | Strips any captured env-variable value. |
| `absolute-paths` | `/Users/alice/...` → `~/.../`; other absolute paths → `<repo-root>/...`. |
| `email` | Email addresses → `[REDACTED-EMAIL]`. |
| `system-prompt` | Strips `system` events entirely (often contain tool definitions / org-specific context). |
| `attachments` | All attachments → `[REDACTED-FILE]`. |
| `tool-args` | Strips `tool.args`; keeps tool name + result. |
| `tool-results` | Strips `tool.result`; keeps tool name + args. |

### Custom rules

Regex + replacement, via config:

```toml
[redaction.custom]
internal-hostname = { pattern = '"host":"(prod-[a-z0-9]+\\.internal)"', replacement = '"host":"<REDACTED-HOSTNAME>"' }
```

### Default profiles

- **`safe-share`** (default for `--public`): `secrets,absolute-paths`. Conservative; doesn't break the transcript's usefulness.
- **`fully-public`** (suggested for blogs / external audiences): `secrets,absolute-paths,env,email,system-prompt`.
- **`minimal`**: nothing. For private shares to trusted recipients.

### Preview

Always available before publishing:

```
$ resleeve session preview-share 01HZ001 --redact=secrets,absolute-paths --format=md > preview.md
$ less preview.md
# inspect; iterate on rules; re-run preview-share; then `share` for real.
```

## Share lifecycle

- **`share.created`** — short_code (Xyz3ABc) generated; row added to DB; redaction applied + snapshot stored separately from the source session (immutable). Domain event fires.
- **`share.expired`** — TTL hits; URL returns 410 Gone. Domain event fires. Snapshot retained for an audit window (default 30d) then GC'd.
- **`share.deleted`** — `resleeve session unshare <id|shortcode>`. URL returns 410 immediately. Snapshot deleted. Domain event fires.

**Key contract:** a share is a *snapshot at creation time*. Modifications to the source session don't affect existing shares. Deleting the source session doesn't break or update existing shares — they remain available until TTL hit or manual revoke.

## Access control

Three levels:

- **`--public`**: anyone with URL can view.
- **`--password=<pw>`**: anyone with URL + password can view. Password is *not* sent to server; recipient enters in browser, used as part of decryption.
- **`--allowlist=user1,user2`** (v2+): requires authenticated resleeve viewer in the list. Federated identity (user1@server1, user2@server2).

Default behavior:

- `--ttl <= 24h` and no `--public` or `--password` flag → require password (auto-generated, shown once).
- `--ttl <= 7d` with `--public` → warn but allow.
- `--ttl > 7d` with `--public` and no `--password` → require explicit `--public --i-understand-its-public`.

## Sad paths

### S1. Secret leaked through default redaction

Default rules missed a domain-specific pattern (internal hostname format, internal tool name, etc.).

- `resleeve session preview-share` shows redacted view *before* generating the link.
- User iterates on rules (built-in flags or custom config), re-previews, then publishes.
- After publishing: `resleeve session unshare <shortcode>` to revoke if discovered too late.

### S2. User deletes the original session

Share survives. Snapshot is independent.

### S3. Recipient wants to import into their own resleeve

The web view shows a "Fork into your resleeve" button:

```
$ resleeve session import https://resleeve.example.com/s/Xyz3ABc
  ✓ Imported as session 01J5K... under scope `imported/Xyz3ABc`
  ✓ Source share preserved as metadata; vendor-opaque fields not transferred.
```

The imported session is a regular resleeve session in the recipient's installation. Read-only flag set; can be cloned to a new session for active continuation.

### S4. Abusive shares / rate limiting

Public shares = potential abuse vector (someone makes 10,000 shares to use resleeve as a CDN).

- Rate limit: max 100 active public shares per user (configurable per deployment).
- Total active shares quota across deployment (operator-configurable).
- Subscription metrics expose share creation rate for ops monitoring.

### S5. Embedding in GitHub / Slack

For URL previews to look nice when pasted:

- OpenGraph metadata in the share HTML (`og:title`, `og:description`, `og:image`).
- Image: server-rendered card with session title, CLI logo, event count, model name.
- GitHub auto-embeds via OG; Slack auto-unfurls.

## What's actually NEW vs. fleet-operator journey

- The entire share subsystem (redaction engine, snapshot model, share lifecycle).
- Multiple output formats (HTML / Markdown / tarball).
- Recipient-side import flow.
- Public-share abuse mitigation (rate limiting, quotas).

## Open questions

- **Signed shares:** resleeve server signs the share payload with its own key; recipient can verify provenance ("yes, this came from this resleeve deployment"). Useful for blog embeds and ecosystem trust. Lean: v2.
- **Replayable shares:** interactive replay with original-timing playback ("asciinema for AI sessions"). Lean: v2.
- **Cross-deployment imports:** when importing from `resleeve.alice.com/s/...` into `resleeve.bob.com`, do we need a federation handshake? Lean: no — public shares are anonymous fetches. Trust is recipient's responsibility.
- **Cost containment per host:** a malicious user creating thousands of shares can DOS the server with traffic. Tie share endpoints to a CDN-friendly cache layer (immutable content, content-addressed)?
- **Markdown export format:** target consumers (GitHub Markdown vs. CommonMark) have different table / code-block flavors. Pick one (lean: GitHub-Flavored).
- **Encryption of share snapshots:** stored encrypted with a share-specific key; `--password=` derives the access key. `--public` shares use a publicly-derivable key (which means... not really encrypted, but at-rest format is the same). Worth detailing in round 3.
- **"Anonymous" vs. "attributed" shares:** does the recipient see *who* shared (resleeve handle, GitHub username)? Lean: default attributed, `--anonymous` flag to hide.
