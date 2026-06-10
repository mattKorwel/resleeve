package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// --- brain + membership management (round-11b, tier 4 "sharing") ---
//
// These endpoints let a user create a SHARED brain and manage its
// membership. Authenticated by the per-device bearer (resolved to a real
// user via deviceFromBearer); the legacy no-user bearer is rejected, like
// requireBrain. Authz model (round-11 "keep it dumb"):
//   - any member may LIST a brain's members + their own brains;
//   - only the OWNER (brains.owner_user_id) may ADD or REMOVE members;
//   - the owner may not remove themselves (no orphaned brain).
// There are no per-member roles yet — every member reads and writes.

// --- wire types ---

// CreateBrainReq is the body of POST /v1/brains.
type CreateBrainReq struct {
	Name string `json:"name"`
}

// CreateBrainResp returns the new brain's id.
type CreateBrainResp struct {
	BrainID string `json:"brain_id"`
}

// BrainView is one row in GET /v1/brains. Role is "owner" when the caller
// owns the brain, else "member".
type BrainView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Owner string `json:"owner_user_id"`
	Role  string `json:"role"`
}

// ListBrainsResp is the body of GET /v1/brains.
type ListBrainsResp struct {
	Brains []BrainView `json:"brains"`
}

// AddMemberReq is the body of POST /v1/brains/{id}/members.
type AddMemberReq struct {
	UserID string `json:"user_id"`
}

// ListMembersResp is the body of GET /v1/brains/{id}/members.
type ListMembersResp struct {
	Members []string `json:"members"`
}

// SetPolicyReq is the body of PUT /v1/brains/{id}/policy (round-12 Part A
// slice 3). The owner flips the brain's encryption_policy — e.g. back to
// 'e2e' (zero-knowledge) on a trusted multi-tenant server.
type SetPolicyReq struct {
	EncryptionPolicy string `json:"encryption_policy"`
}

// --- route registration ---

// registerBrainRoutes wires the brain management endpoints. Called from
// registerAuthRoutes (only when an identity store is configured).
func (s *Server) registerBrainRoutes() {
	s.mux.HandleFunc("POST /v1/brains", s.requireUser(s.handleCreateBrain))
	s.mux.HandleFunc("GET /v1/brains", s.requireUser(s.handleListBrains))
	s.mux.HandleFunc("GET /v1/brains/{id}/members", s.requireUser(s.handleListMembers))
	s.mux.HandleFunc("POST /v1/brains/{id}/members", s.requireUser(s.handleAddMember))
	s.mux.HandleFunc("DELETE /v1/brains/{id}/members/{user_id}", s.requireUser(s.handleRemoveMember))
	s.mux.HandleFunc("PUT /v1/brains/{id}/policy", s.requireUser(s.handleSetPolicy))
}

// requireUser gates a handler on a per-device bearer that resolves to a
// real user, rejecting the legacy no-user bearer (mirrors requireBrain's
// identity check). The resolved device is passed to the handler.
func (s *Server) requireUser(next func(http.ResponseWriter, *http.Request, *rsql.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := s.deviceFromBearer(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if dev.UserID == "" {
			writeError(w, http.StatusUnauthorized, "endpoint requires a per-device token; legacy bearer cannot identify a user")
			return
		}
		if s.brains == nil || s.memberships == nil {
			writeError(w, http.StatusServiceUnavailable, "brain store not configured")
			return
		}
		next(w, r, dev)
	}
}

// --- handlers ---

func (s *Server) handleCreateBrain(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	defer r.Body.Close()
	var req CreateBrainReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode: %v", err))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing brain name")
		return
	}
	// round-15 (M-C): cap how many brains one user may own. Each brain
	// provisions a DEK row + a membership row; without a cap a script can
	// mint brains without bound. We count the caller's currently-owned
	// brains (which includes their auto-provisioned personal brain) and
	// reject 429 at/over the cap.
	owned, err := s.brains.ListByOwner(r.Context(), dev.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "count owned brains: "+err.Error())
		return
	}
	if len(owned) >= s.maxBrainsPerUser {
		writeError(w, http.StatusTooManyRequests,
			fmt.Sprintf("brain limit reached (max %d per user)", s.maxBrainsPerUser))
		return
	}
	now := time.Now().UTC()
	b := &rsql.Brain{
		ID:          newID(16),
		Name:        name,
		Kind:        rsql.BrainKindShared,
		OwnerUserID: dev.UserID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.brains.Create(r.Context(), b); err != nil {
		writeError(w, http.StatusInternalServerError, "create brain: "+err.Error())
		return
	}
	// The creator is the first member (and owner). Membership is the
	// access edge — without this row the owner couldn't touch their own
	// brain.
	if err := s.memberships.Add(r.Context(), &rsql.Membership{
		BrainID:   b.ID,
		UserID:    dev.UserID,
		CreatedAt: now,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "add owner membership: "+err.Error())
		return
	}
	// Server-at-rest (round-12 Part A): provision a per-brain DEK wrapped
	// under the operator master key. No-op when no master key is set.
	if err := s.provisionBrainKey(r.Context(), b.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "provision brain key: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, CreateBrainResp{BrainID: b.ID})
}

func (s *Server) handleListBrains(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	brains, err := s.brains.ListForUser(r.Context(), dev.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list brains: "+err.Error())
		return
	}
	views := make([]BrainView, 0, len(brains))
	for _, b := range brains {
		role := "member"
		if b.OwnerUserID == dev.UserID {
			role = "owner"
		}
		views = append(views, BrainView{
			ID:    b.ID,
			Name:  b.Name,
			Kind:  string(b.Kind),
			Owner: b.OwnerUserID,
			Role:  role,
		})
	}
	writeJSON(w, http.StatusOK, ListBrainsResp{Brains: views})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	brainID := r.PathValue("id")
	// Any MEMBER may list members; a non-member gets 403 (and learns
	// nothing about the brain's existence beyond "not yours").
	ok, err := s.memberships.IsMember(r.Context(), dev.UserID, brainID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "membership check: "+err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "not a member of this brain")
		return
	}
	members, err := s.memberships.ListMembers(r.Context(), brainID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list members: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ListMembersResp{Members: members})
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	defer r.Body.Close()
	brainID := r.PathValue("id")
	if _, ok := s.requireOwner(w, r, dev, brainID); !ok {
		return
	}
	var req AddMemberReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode: %v", err))
		return
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		writeError(w, http.StatusBadRequest, "missing user_id")
		return
	}
	// The added user must exist (else a typo silently grants a phantom
	// membership). serverUsers is always set alongside brains here.
	if s.serverUsers != nil {
		if _, err := s.serverUsers.GetByID(r.Context(), userID); err != nil {
			if errors.Is(err, rsql.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "no such user")
				return
			}
			writeError(w, http.StatusInternalServerError, "lookup user: "+err.Error())
			return
		}
	}
	if err := s.memberships.Add(r.Context(), &rsql.Membership{
		BrainID:   brainID,
		UserID:    userID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "add member: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	brainID := r.PathValue("id")
	target := r.PathValue("user_id")
	b, ok := s.requireOwner(w, r, dev, brainID)
	if !ok {
		return
	}
	// Owner-cannot-orphan guard: the owner may not remove themselves,
	// which would leave the brain memberless (and ownerless in practice).
	// Ownership transfer / brain deletion is a later refinement.
	if target == b.OwnerUserID {
		writeError(w, http.StatusBadRequest, "owner cannot be removed (would orphan the brain)")
		return
	}
	if err := s.memberships.Remove(r.Context(), brainID, target); err != nil {
		writeError(w, http.StatusInternalServerError, "remove member: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetPolicy flips a brain's encryption_policy (round-12 Part A
// slice 3). Owner-only (requireOwner → 404 unknown / 403 not owner).
// Validation: 'e2e' is only allowed on a PERSONAL brain — end-to-end on a
// shared brain needs group keys (not built yet), so it's a 400.
// 'server-side' is always allowed.
//
// Green-field note: switching an existing server-side brain to e2e does
// NOT re-encrypt rows already at rest under the server DEK. Only NEW client
// writes get sealed (the daemon re-samples shouldSeal at its next whoami
// handshake). See docs/design/round-12/04-e2e-opt-in-slice.md.
func (s *Server) handleSetPolicy(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	defer r.Body.Close()
	brainID := r.PathValue("id")
	b, ok := s.requireOwner(w, r, dev, brainID)
	if !ok {
		return
	}
	var req SetPolicyReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode: %v", err))
		return
	}
	policy := rsql.EncryptionPolicy(strings.TrimSpace(req.EncryptionPolicy))
	switch policy {
	case rsql.EncryptionPolicyServerSide:
		// Always allowed.
	case rsql.EncryptionPolicyE2E:
		// e2e is zero-knowledge: the server holds no key, so a shared brain
		// would need group keys to let members read each other's writes.
		// Not built yet — reject rather than silently lock out members.
		if b.Kind != rsql.BrainKindPersonal {
			writeError(w, http.StatusBadRequest,
				"end-to-end encryption on a shared brain needs group keys; not supported yet")
			return
		}
	default:
		writeError(w, http.StatusBadRequest,
			"invalid encryption_policy (want \"server-side\" or \"e2e\")")
		return
	}
	if err := s.brains.UpdatePolicy(r.Context(), brainID, policy); err != nil {
		writeError(w, http.StatusInternalServerError, "update policy: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireOwner loads the brain and asserts the caller is its owner. On any
// failure it writes the appropriate error (404 unknown brain, 403 not
// owner) and returns ok=false. We return 403 (not 404) for a brain the
// caller can see but doesn't own; an unknown brain is 404.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request, dev *rsql.Device, brainID string) (*rsql.Brain, bool) {
	b, err := s.brains.Get(r.Context(), brainID)
	if err != nil {
		if errors.Is(err, rsql.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such brain")
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, "lookup brain: "+err.Error())
		return nil, false
	}
	if b.OwnerUserID != dev.UserID {
		writeError(w, http.StatusForbidden, "only the brain owner can manage members")
		return nil, false
	}
	return b, true
}
