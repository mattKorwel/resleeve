package serve

import (
	"context"
	"errors"
	"net/http"
	"strings"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// brainCtxKey is the request-context key under which requireBrain stashes
// the resolved (device, brainID) pair for the sync handlers. Private type
// so no other package can collide on the key.
type brainCtxKey struct{}

// brainContext is what requireBrain resolves and hands to the sync
// handlers via the request context: the authenticated device (carrying
// UserID) and the brain id the caller is acting in (already validated
// for membership, or "" in single-tenant mode).
type brainContext struct {
	dev     *rsql.Device
	brainID string
}

// --- keyspace partitioning ---
//
// The on-the-wire keyspace the client uses is brain-agnostic
// ("sessions/<id>", "memory/<scope>/..."). The server transparently
// PREPENDS the acting brain ("<brain_id>/sessions/<id>") before storing
// and STRIPS it before returning, so the daemon's ingest + cursor logic
// stays unchanged across the single→multi-tenant flip. In single-tenant
// mode (--single-tenant) the prefix is empty and the keyspace is global,
// preserving the legacy tier-1/2 solo behavior.

// scopeKey prepends the brain prefix to a client-supplied key. brainID
// "" (single-tenant mode) returns the key unchanged.
func scopeKey(brainID, key string) string {
	if brainID == "" {
		return key
	}
	return brainID + "/" + key
}

// unscopeKey strips the brain prefix from a stored key, returning the
// brain-agnostic key the client expects. brainID "" returns the key
// unchanged. A key that doesn't carry the expected prefix is returned
// as-is (defensive — should not happen for keys this server stored).
func unscopeKey(brainID, key string) string {
	if brainID == "" {
		return key
	}
	prefix := brainID + "/"
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return key
}

// scopePrefix builds the List prefix for a (brain, kind) pair: the brain
// namespace joined with the wire kind. Single-tenant mode returns just
// the kind, matching the legacy global keyspace.
func scopePrefix(brainID, kind string) string {
	if brainID == "" {
		return kind
	}
	return brainID + "/" + kind
}

// --- middleware ---

// requireBrain authenticates the per-device bearer, resolves the brain
// the caller is acting in (the ?brain=<id> selector, defaulting to the
// caller's personal brain), validates membership, and stashes the
// (device, brainID) pair in the request context for the wrapped handler.
//
// Legacy no-user bearers are rejected (a content-blind cross-tenant hole
// otherwise): in multi-tenant mode every sync call must identify a user.
// The exception is single-tenant mode, where there is exactly one user's
// data and no brain partitioning — the legacy bearer is accepted and the
// resolved brainID is "".
func (s *Server) requireBrain(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := s.deviceFromBearer(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}

		// Single-tenant mode (tiers 1–2 solo self-host): no brain
		// partitioning, legacy bearer permitted, keyspace stays global.
		if s.singleTenant {
			ctx := context.WithValue(r.Context(), brainCtxKey{}, brainContext{dev: dev, brainID: ""})
			next(w, r.WithContext(ctx))
			return
		}

		// Multi-tenant: a real user identity is required.
		if dev.UserID == "" {
			writeError(w, http.StatusUnauthorized, "endpoint requires a per-device token; legacy bearer cannot identify a user")
			return
		}
		if s.brains == nil || s.memberships == nil {
			writeError(w, http.StatusServiceUnavailable, "brain store not configured")
			return
		}

		brainID, err := s.resolveBrain(r.Context(), dev.UserID, r.URL.Query().Get("brain"))
		if err != nil {
			switch {
			case errors.Is(err, errBrainForbidden):
				writeError(w, http.StatusForbidden, "not a member of the requested brain")
			case errors.Is(err, errNoPersonalBrain):
				writeError(w, http.StatusInternalServerError, "no personal brain provisioned for user")
			default:
				writeError(w, http.StatusInternalServerError, "resolve brain: "+err.Error())
			}
			return
		}
		ctx := context.WithValue(r.Context(), brainCtxKey{}, brainContext{dev: dev, brainID: brainID})
		next(w, r.WithContext(ctx))
	}
}

var (
	// errBrainForbidden: the caller asked for a brain they're not a member of.
	errBrainForbidden = errors.New("serve: caller is not a member of the requested brain")
	// errNoPersonalBrain: no brain selector and the user has no personal brain.
	errNoPersonalBrain = errors.New("serve: user has no personal brain")
)

// resolveBrain maps a (userID, selector) pair to the brain id the caller
// acts in. When selector is non-empty it must name a brain the user is a
// member of (else errBrainForbidden). When empty it defaults to the
// caller's personal brain (their first owned personal brain).
func (s *Server) resolveBrain(ctx context.Context, userID, selector string) (string, error) {
	if selector != "" {
		ok, err := s.memberships.IsMember(ctx, userID, selector)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errBrainForbidden
		}
		return selector, nil
	}
	// Default: the user's personal brain.
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

// brainFromContext recovers the (device, brainID) pair requireBrain
// stashed. ok is false when the handler wasn't wrapped by requireBrain
// (a programming error).
func brainFromContext(r *http.Request) (brainContext, bool) {
	bc, ok := r.Context().Value(brainCtxKey{}).(brainContext)
	return bc, ok
}
