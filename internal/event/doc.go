// Package event defines the session-event types flowing through resleeve.
// Hybrid envelope: normalized `content` (OTel-GenAI-aligned) + vendor-faithful
// `vendor` block (the raw vendor record for same-CLI reconstruction).
// See docs/design/round-2/04-event-schema.md.
package event
