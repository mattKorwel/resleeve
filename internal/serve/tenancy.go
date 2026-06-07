package serve

import (
	"context"
	"errors"
	"net/http"
	"strings"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// --- round-11a slice 2: multi-tenant keyspace partitioning + read authz ---
//
// The server partitions the blob keyspace by *brain* (a server-side
// storage namespace), NOT by user. The client wire protocol is unchanged:
// it keeps speaking the kind-scoped keyspace (`sessions/<id>`,
// `events/...`, `memory/...`) and never learns about brain ids. The server
// transparently injects a `<brain_id>/` prefix on push and strips it on
// pull/SSE, so two users acting in two different personal brains are fully
// isolated even though the bytes on disk live under one backend.
//
// Acting brain resolution: the per-device bearer resolves to a *rsql.Device
// carrying UserID; the default acting brain is that user's *personal*
// brain. An optional `?brain=<id>` selector is validated via
// Memberships.IsMember (403 on non-member) — the seam for shared-brain
// selection in a later slice; personal-default is the only path exercised
// here.
//
// --single-tenant mode disables all of the above: no brain prefixing, and
// the legacy no-user bearer keeps working on /v2/sync/*. That's the solo
// self-hoster (tiers 1–2) escape hatch.

// errNoPersonalBrain is returned by resolveBrainID when an authenticated
// user has no personal brain. In practice register always provisions one,
// so this indicates a corrupt/partial account rather than a normal state.
var errNoPersonalBrain = errors.New("serve: user has no personal brain")

// multiTenant reports whether brain-scoped partitioning + read authz are
// active. It is the inverse of the --single-tenant escape hatch AND
// requires the brain stores to be wired — without them there is nothing
// to resolve an acting brain from, so we fall back to single-tenant
// (content-blind) behavior rather than failing every sync call.
func (s *Server) multiTenant() bool {
	if s.singleTenant {
		return false
	}
	return s.brains != nil && s.memberships != nil
}

// requireBrain is the sync-route middleware for multi-tenant mode. It
// resolves the bearer to a real per-device token (rejecting the legacy
// no-user bearer, which cannot identify a tenant — that's the cross-tenant
// hole this slice closes), then resolves the acting brain and passes both
// to the handler. In single-tenant mode the server never installs this
// middleware; it uses requireAuth (legacy bearer OK, no prefixing).
func (s *Server) requireBrain(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := s.deviceFromBearer(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		// The legacy single-bearer fallback resolves to a synthetic device
		// with UserID="". In multi-tenant mode that bearer cannot be mapped
		// to a tenant, so it must be rejected on sync routes — accepting it
		// would let a content-blind token read/write across every brain.
		if dev == nil || dev.UserID == "" {
			writeError(w, http.StatusForbidden, "legacy bearer cannot identify a tenant; use a per-device token or run the server with --single-tenant")
			return
		}
		brainID, err := s.resolveBrainID(r.Context(), r, dev.UserID)
		if err != nil {
			switch {
			case errors.Is(err, errNoPersonalBrain):
				writeError(w, http.StatusForbidden, "no personal brain for this user")
			case errors.Is(err, errBrainNotMember):
				writeError(w, http.StatusForbidden, "not a member of the requested brain")
			default:
				writeError(w, http.StatusInternalServerError, "resolve brain: "+err.Error())
			}
			return
		}
		next(w, r, brainID)
	}
}

// errBrainNotMember is returned by resolveBrainID when a ?brain=<id>
// selector names a brain the authenticated user is not a member of.
var errBrainNotMember = errors.New("serve: not a member of requested brain")

// resolveBrainID determines which brain the authenticated user is acting
// in for this request. Default: the user's personal brain. Optional
// `?brain=<id>` selector: validated via Memberships.IsMember (the seam for
// shared-brain selection; only personal-default is exercised this slice).
func (s *Server) resolveBrainID(ctx context.Context, r *http.Request, userID string) (string, error) {
	if sel := strings.TrimSpace(r.URL.Query().Get("brain")); sel != "" {
		ok, err := s.memberships.IsMember(ctx, userID, sel)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errBrainNotMember
		}
		return sel, nil
	}
	return s.personalBrainID(ctx, userID)
}

// personalBrainID returns the id of the user's personal brain. A user has
// exactly one (auto-provisioned on register). If the user owns several
// brains (e.g. they later created a shared brain), we pick the one whose
// Kind is personal; ties shouldn't happen given the register invariant.
func (s *Server) personalBrainID(ctx context.Context, userID string) (string, error) {
	owned, err := s.brains.ListByOwner(ctx, userID)
	if err != nil {
		return "", err
	}
	for _, b := range owned {
		if b.Kind == rsql.BrainKindPersonal {
			return b.ID, nil
		}
	}
	return "", errNoPersonalBrain
}

// brainKey injects the brain namespace in front of a client key. The
// client sends `sessions/<id>`; storage holds `<brain_id>/sessions/<id>`.
// An empty brainID (single-tenant mode) is a no-op: the storage key equals
// the client key (the pre-slice-2 flat layout). Keys are '/'-joined storage
// keys, NOT OS paths — do not use filepath.
func brainKey(brainID, clientKey string) string {
	if brainID == "" {
		return clientKey
	}
	return brainID + "/" + clientKey
}

// brainPrefix is the List prefix for a (brain, kind) pair: the storage
// namespace under which all of that brain's rows for that kind live. An
// empty brainID (single-tenant) yields just the kind — the legacy prefix.
func brainPrefix(brainID, kind string) string {
	if brainID == "" {
		return kind
	}
	return brainID + "/" + kind
}

// stripBrainPrefix removes the leading `<brain_id>/` from a storage key so
// the client receives the unchanged kind-scoped key. An empty brainID
// (single-tenant) returns the key unchanged. Returns ok=false if a non-empty
// prefix is expected but absent — a guard against ever leaking another
// brain's row to the client (the caller drops such rows defensively, though
// List is already prefix-scoped).
func stripBrainPrefix(brainID, storageKey string) (string, bool) {
	if brainID == "" {
		return storageKey, true
	}
	pfx := brainID + "/"
	if !strings.HasPrefix(storageKey, pfx) {
		return storageKey, false
	}
	return strings.TrimPrefix(storageKey, pfx), true
}
