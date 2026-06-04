# resleeve MCP server

You have access to **resleeve**, a self-hosted memory + session service
organized into hierarchical scopes. Each scope holds a **plan** (named
slots of markdown — `_default` by convention) and an append-only
**learning** log. Plans and learnings inherit down the scope tree;
a `.donotinherit` marker on a scope is a boundary reset.

## Standing instructions

### Curation is operator-driven

**Never auto-curate.** Memory is a high-signal substrate, not a chat
log. Only call the write tools when the user explicitly asks — phrases
like "save this as a learning", "update the plan", "remember that…"
are the trigger. A summary of this session's work is **not** a learning
unless the user says "save that".

### Plans are named slots

- `resleeve_plan_write` **overwrites** the slot. Default slot name is
  `_default`. Use named slots for sibling drafts (e.g. `next-quarter`).
- `resleeve_plan_read` fetches one slot. `resleeve_plan_list` shows
  every slot at a scope.
- Structure plans as `## Now`, `## Next`, `## Open questions` unless
  another shape is already in place — preserve the user's structure.

### Learnings are append-only with soft-supersede

- `resleeve_learning_append` writes a new entry; never edits an
  existing one. One finding per call. Be concise — durable
  conclusions, not blow-by-blow.
- To correct a prior entry, pass `supersedes_id` with that entry's id.
  The old entry stays in storage (audit trail) but is hidden from
  default reads. Only supersede when the prior entry is actually
  wrong; don't supersede to consolidate.

### Scope hygiene

- `resleeve_scope_set` creates or updates a scope. Prefer existing
  scopes; only create new ones at the user's request.
- `resleeve_scope_list` shows the full tree; `resleeve_scope_get`
  inspects one node.
- `resleeve_scope_delete` refuses (409) if the scope has children.
- Scope kinds are: portfolio | program | project | dispatch | agent |
  other. Ask if unsure.

### Reading context

- `resleeve_context` returns the rolled-up markdown (parent plans +
  learnings, walked shallow→deep, truncated at `do_not_inherit`
  boundaries). Use it when you need the full inherited frame.
- For a single layer, use `resleeve_plan_read` / `resleeve_learning_list`
  with explicit scope.

### Don't

- Don't auto-save without explicit operator ask.
- Don't write to underscore-prefixed scopes (reserved).
- Don't fabricate scope paths; use `resleeve_scope_list` to discover.
- Don't supersede learnings just to "tidy up" — operator decides.
