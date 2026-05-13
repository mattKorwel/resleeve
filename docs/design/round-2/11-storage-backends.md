# Storage backends

Resleeve has two distinct durability stories:

- **SQL data plane** — sessions, events, slots, subscriptions, shares, users, audit. **Pluggable.**
- **Blob store** — content-addressed binary content (large attachments, externalized `vendor.native_payload`). **Pluggable.**
- **Memory module (v2+)** — plans, learnings, scope tree. Git-backed per ori's existing model. Different concern; not covered by this doc.

The SQL data plane is pluggable so anyone deploying resleeve on a cloud VM with a managed database can point at it without forking. **SQLite for OSS self-host → Cloud Postgres for fleet/cloud → document-native (Firestore / Mongo) as a separate adapter family later if there's demand.**

## SQL backends

| Backend | Deployed | Notes |
|---|---|---|
| **SQLite 3.40+** | **v1** | OSS starter. Zero-config, single file, WAL mode by default. The "just install and run" deployment. |
| **PostgreSQL 14+** | **v2** | One implementation covers Cloud SQL for Postgres, **AlloyDB**, **Spanner (Postgres-interface, GA since 2023)**, RDS, Supabase, Neon, Crunchy, on-prem. Set `RESLEEVE_DATABASE_URL=postgres://...` and go. |
| **MySQL 8+ / MariaDB 10.5+** | v2+ | Cloud SQL, RDS, PlanetScale. Demand-driven. |
| **Turso / libSQL** | v2+ | Managed SQLite with embedded replicas. |
| **CockroachDB / TiDB / YugabyteDB** | v2+ | Postgres-wire-compatible; ships if user demand emerges. |

The Postgres abstraction *exists in code from v1* (storage interface sized for it from the start). The v1 *deployment* is SQLite-only; Postgres becomes a supported deployment in v2 when the remote server arrives.

**Out of scope (SQL family):** DynamoDB and other KV stores — different paradigm; document family below.

## GCP deployment path

GCP is a primary target. The path is straightforward because every GCP-managed SQL option speaks Postgres:

| GCP target | Reaches resleeve via | When |
|---|---|---|
| **Cloud SQL for Postgres** | Standard `postgres://` URL | v2 (the canonical "I'm on GCP" deployment) |
| **AlloyDB** | Standard `postgres://` URL (Postgres-compatible) | v2 |
| **Spanner (Postgres interface)** | `postgres://` pointing at the Spanner endpoint | v2 — works because we follow Postgres-portable patterns |
| **Firestore** | Separate document adapter | v3+ (different family) |
| **Cloud Storage** | Blob driver — S3-compatible HMAC, or native GCS | v2 (with the remote server) |
| **Workload Identity** | Connection-string credential provider plugin | v2+ |

### Spanner-Postgres-interface caveats

Spanner-Postgres is a subset of Postgres; resleeve's portability rules already accommodate the gaps:

- **No `LISTEN/NOTIFY`** — not used (we use SSE / polling in app layer).
- **No stored procedures / triggers** — not used.
- **Limited `JSON` operators** — already abstracted behind a portable jsonpath wrapper.
- **No native FTS** — neither does Spanner; FTS is plugged in via the `FullTextIndex` abstraction (Typesense as external default for non-FTS-native backends).
- **TrueTime / external consistency** — different transaction model; app-level invariants still hold.
- **Per-node pricing** — expensive at small scale; appropriate for multi-region team deployments.

Spanner is the right backend if you're already on it for global-multi-region reasons. For single-team deployments, Cloud SQL Postgres is right-sized.

## Document-store adapter family (v3+)

Resleeve's data character (sessions containing events, append-mostly, nested content) is *also* document-shaped. A document backend is viable and may be right for some deployments — but it's a different interface, not a swappable SQL backend.

Planned (v3+, demand-driven):

| Backend | Notes |
|---|---|
| **Firestore** | GCP-native. Serverless. Native real-time listeners replace some SSE machinery. GCP-only. |
| **MongoDB** (self-host or Atlas) | Vendor-agnostic. Better for "I want document but no cloud lock-in." |

What changes when going document:

- Different storage interface (`internal/storage/doc/...` peer to `internal/storage/sql/...`).
- Full-text search via external indexer (Typesense, Algolia, Elasticsearch).
- Different operational tooling.
- No joins or `RETURNING` — code assumes read-after-write.

We don't pretend a document backend is a drop-in replacement for SQL — it's a separate module that implements the same domain interfaces via document-shaped storage.

## Configuration

Single env var (or config file key):

```
RESLEEVE_DATABASE_URL=sqlite:///var/resleeve/data.db
RESLEEVE_DATABASE_URL=postgres://user:pw@host:5432/resleeve?sslmode=verify-full
RESLEEVE_DATABASE_URL=postgres://...@spanner-projects-myproj-instance-myinst.svc.spanner.googleapis.com/...
RESLEEVE_DATABASE_URL=mongodb://user:pw@host:27017/resleeve      # v3+
RESLEEVE_DATABASE_URL=firestore://my-gcp-project/resleeve         # v3+
```

Default if unset: `sqlite:///$XDG_DATA_HOME/resleeve/data.db`.

## Schema portability rules (SQL family)

Hard rules for every PR that touches schema or queries:

- **Types only:** `TEXT`, `INTEGER`, `BLOB`, `JSON` (as `TEXT` containing JSON where `JSON` type isn't standard).
- **Primary keys:** ULIDs in `TEXT(26)`. No `AUTO_INCREMENT` / `SERIAL`.
- **Timestamps:** ISO 8601 strings in `TEXT`. Avoid `TIMESTAMP` / `DATETIME` (TZ semantics differ).
- **No backend-specific features:**
  - No SQLite `WITHOUT ROWID`, `STRICT`, recursive CTEs in hot paths.
  - No Postgres `JSONB` operators (`->`, `->>`) in queries; use the portable jsonpath wrapper.
  - No MySQL stored procedures / triggers.
  - No `LISTEN/NOTIFY` (Spanner-Postgres lacks it; we don't use it anyway).
- **JSON queries:** `internal/storage/sql/jsonpath` wrapper translates abstract path expressions to backend syntax (SQLite `json_extract`, Postgres `jsonb_path_query`, etc.).
- **Indexes:** every perf-critical query has explicit indexes; same DDL across backends.

### Full-text search

Each backend has its own FTS story:

- SQLite: FTS5 virtual tables.
- Postgres / AlloyDB: `tsvector` columns with `GIN` indexes.
- Spanner-Postgres: no native FTS — use external indexer.
- MySQL: `FULLTEXT` indexes on InnoDB.

Abstracted behind a `FullTextIndex` interface. Per-backend implementations; query API is uniform.

## Migrations

- Single migration tool: `golang-migrate` (lean; alternative: `goose`).
- Migration files: SQL, one set per backend where dialect differences are unavoidable (rare — most schemas shared via `*.shared.sql`).
- `resleeve serve` auto-applies on startup. `RESLEEVE_AUTO_MIGRATE=false` disables for ops who want manual windows.
- Forward-only by default. Down-migrations for development; production doesn't roll back without explicit approval.

## Blob storage

| Driver | Deployed | Notes |
|---|---|---|
| **Local filesystem** | **v1** | `var/blobs/sha256/aa/bb/...`. Two-level directory sharding for filesystem-friendliness. |
| **S3-compatible** | v2 | S3, MinIO, R2, **GCS via S3 HMAC API**, DigitalOcean Spaces, Backblaze B2. |
| **GCS native** | v2+ | Native client; auth via service account or Workload Identity. |
| **Azure Blob** | v2+ | Demand-driven. |

GCP users have two paths: S3-compatible HMAC (works without GCS SDK) or native GCS client (gets Workload Identity for free). Lean: ship S3-compatible first; add native GCS when there's a reason.

## What stays SQLite-only

- **Daemon-local event buffer** (`var/resleeve/buffer/<session_id>.events`) is always SQLite or a flat append-only JSONL file. Pod-lifetime; no need for "real" durability.
- **Single-machine standalone mode** defaults SQLite throughout — no nudge to set up Postgres until the user moves to cross-machine (v2).

## Cloud-Postgres auth flavors

Managed Postgres providers each have quirks:

- **Standard password / SSL:** works everywhere — `postgres://user:pw@host:5432/...`.
- **AWS RDS IAM auth:** runtime credential rotation (15-min tokens). Driver-wrapper pattern.
- **GCP Cloud SQL Auth Proxy / Workload Identity:** out-of-process proxy on `127.0.0.1:5432`, or in-process via Workload Identity binding. Both work without resleeve code changes.
- **Azure AD auth:** AAD token retrieval per connection.
- **mTLS:** standard libpq SSL flags.

Lean: v1 ships standard password + SSL. IAM / AD / Workload Identity arrive as a "credential provider" plugin in v2 when the first user needs them.

## Encryption at rest

Per `10-auth-subsystem.md`: resleeve does data-at-rest encryption at the **application layer** (KEK-encrypted ciphertext stored as blobs). The SQL backend itself doesn't need to provide encryption.

Cloud users may *also* want backend-side encryption (RDS encryption, Cloud SQL CMEK, Spanner CMEK) — additive, operator-configurable, not resleeve's concern.

## What this implies for v1

- **Storage abstraction** lives in `internal/storage/sql/` behind thin interfaces (`SessionStore`, `EventStore`, `SubscriptionStore`, etc.). Sized for Postgres from day one.
- **SQLite implementation** is the only SQL backend *deployed* in v1.
- **Schema portability rules** are enforced from day one via lint + reviewer checklist (catches backend-specific SQL before merge).
- **Document adapter family** is not designed in v1 (would add complexity without payoff); the SQL interfaces are domain-typed enough that a separate document family can be added cleanly later.

## Open questions

- **Connection pooling tuning:** SQLite serializes writes; Postgres benefits from pools. Per-backend defaults sized via benchmark.
- **Postgres-specific optimizations** (`JSONB`, `tsvector`, array columns): feature-detect at connect time and opt into faster paths automatically; SQLite path always functional.
- **Cross-backend data export/import:** `resleeve db export` / `resleeve db import` for migrating between backends. Lean: v2.
- **Read-replica support for Postgres:** route reads to replicas. Lean: v2+.
- **Spanner-specific tuning:** write batching matters more on Spanner; expose a per-backend tuning hook in v2 if performance becomes a concern.
- **Hot-reload of `RESLEEVE_DATABASE_URL`:** probably requires graceful restart. Document.
- **Schema linting tool:** `sqruff`, `sqlfluff`, or custom. Decide at v1 repo init.
