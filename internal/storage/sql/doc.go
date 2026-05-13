// Package sql provides backend-agnostic storage interfaces for resleeve's
// session, event, slot, share, subscription, and auth data. Backend
// implementations live in subpackages (sqlite, postgres). v1 deploys
// SQLite; the interfaces are sized for Postgres from day one.
// See docs/design/round-2/11-storage-backends.md.
package sql
