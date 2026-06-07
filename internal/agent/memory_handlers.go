package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/mattkorwel/resleeve/internal/memory"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func (d *Daemon) registerMemoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/scope", d.requireBearer(d.handleScope))
	mux.HandleFunc("/v1/scopes", d.requireBearer(d.handleScopes))
	mux.HandleFunc("/v1/plan", d.requireBearer(d.handlePlan))
	mux.HandleFunc("/v1/plan/versions", d.requireBearer(d.handlePlanVersions))
	mux.HandleFunc("/v1/plans", d.requireBearer(d.handlePlans))
	mux.HandleFunc("/v1/learnings", d.requireBearer(d.handleLearnings))
	mux.HandleFunc("/v1/context", d.requireBearer(d.handleContext))
}

// --- /v1/scope ---

func (d *Daemon) handleScope(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing path")
		return
	}
	switch r.Method {
	case http.MethodPut:
		d.putScope(w, r, path)
	case http.MethodGet:
		d.getScope(w, r, path)
	case http.MethodDelete:
		d.deleteScope(w, r, path)
	default:
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (d *Daemon) putScope(w http.ResponseWriter, r *http.Request, path string) {
	var body struct {
		Kind         string `json:"kind"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cwd          string `json:"cwd"`
		DoNotInherit bool   `json:"do_not_inherit"`
	}
	// Empty body is OK (callers can create a scope with just the path
	// query param + later PUT to set kind); surface other decode errors.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErrorStatus(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	kind := memory.ScopeKind(body.Kind)
	if !kind.Valid() {
		writeErrorStatus(w, http.StatusBadRequest, "invalid kind")
		return
	}
	s := &memory.Scope{
		Path:         path,
		Kind:         kind,
		Title:        body.Title,
		Description:  body.Description,
		Cwd:          body.Cwd,
		DoNotInherit: body.DoNotInherit,
	}
	if err := d.store.Memory().UpdateScope(r.Context(), s); err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	got, err := d.store.Memory().GetScope(r.Context(), path)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Fast-tier sync: enqueue the canonical (post-write) scope so other
	// machines see the same created_at/updated_at we returned to the
	// caller. Enqueue failure is logged but non-fatal — the next
	// write will retry; the local store is already committed.
	if d.sync != nil {
		if err := d.sync.EnqueueScope(r.Context(), got); err != nil {
			logSyncEnqueueErr("scope", path, err)
		}
	}
	writeJSON(w, http.StatusOK, got)
}

func (d *Daemon) getScope(w http.ResponseWriter, r *http.Request, path string) {
	s, err := d.store.Memory().GetScope(r.Context(), path)
	if err != nil {
		if errors.Is(err, rsql.ErrNotFound) {
			writeErrorStatus(w, http.StatusNotFound, "not found")
			return
		}
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (d *Daemon) deleteScope(w http.ResponseWriter, r *http.Request, path string) {
	if err := d.store.Memory().DeleteScope(r.Context(), path); err != nil {
		if errors.Is(err, memory.ErrScopeHasChildren) {
			writeErrorStatus(w, http.StatusConflict, err.Error())
			return
		}
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- /v1/scopes ---

func (d *Daemon) handleScopes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	scopes, err := d.store.Memory().ListScopes(r.Context())
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if scopes == nil {
		scopes = []*memory.Scope{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes})
}

// --- /v1/plan ---

func (d *Daemon) handlePlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing scope")
		return
	}
	slot := q.Get("slot")

	switch r.Method {
	case http.MethodPut:
		d.putPlan(w, r, scope, slot)
	case http.MethodGet:
		inherit := q.Get("inherit") == "true"
		d.getPlan(w, r, scope, slot, inherit)
	default:
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// planConflictBody is the JSON returned with HTTP 409 on a stale write:
// it carries the current HEAD (version + content) so the caller can
// reconcile against it and retry. See
// docs/design/round-12/01-plan-versioning-slice.md.
type planConflictBody struct {
	Error string       `json:"error"`
	Head  *memory.Plan `json:"head,omitempty"`
}

func (d *Daemon) putPlan(w http.ResponseWriter, r *http.Request, scope, slot string) {
	defer r.Body.Close()
	var body struct {
		Content string `json:"content"`
		// BaseVersion is the HEAD version the caller derived from (0 =
		// expect-new). A pointer distinguishes "omitted" from an explicit
		// 0, though both are treated the same unless Force is set.
		BaseVersion *int64 `json:"base_version"`
		Force       bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	base := memory.NewPlanBaseVersion
	if body.BaseVersion != nil {
		base = *body.BaseVersion
	}
	// Daemon is a single-user local process — there is no per-request user
	// identity (the bearer is a shared local secret), so author is the
	// local identity if configured, else empty. round-12B: serve's
	// per-device bearer path stamps the real user; see the slice doc.
	author := d.localAuthor()
	got, err := d.store.Memory().AppendPlanVersion(r.Context(), scope, slot, body.Content, author, base, body.Force)
	if err != nil {
		var conflict *memory.PlanConflictError
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, planConflictBody{
				Error: "plan version conflict: base_version is stale; reconcile against head and retry",
				Head:  conflict.Head,
			})
			return
		}
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d.sync != nil {
		if err := d.sync.EnqueuePlan(r.Context(), got); err != nil {
			logSyncEnqueueErr("plan", scope+"/"+slot, err)
		}
	}
	writeJSON(w, http.StatusOK, got)
}

func (d *Daemon) getPlan(w http.ResponseWriter, r *http.Request, scope, slot string, inherit bool) {
	if !inherit {
		p, err := d.store.Memory().GetPlan(r.Context(), scope, slot)
		if err != nil {
			if errors.Is(err, rsql.ErrNotFound) {
				writeErrorStatus(w, http.StatusNotFound, "not found")
				return
			}
			writeErrorStatus(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	plans := memory.CollectPlanChain(r.Context(), d.store.Memory(), scope, slot)
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

// --- /v1/plans ---

func (d *Daemon) handlePlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing scope")
		return
	}
	ps, err := d.store.Memory().ListPlans(r.Context(), scope)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ps == nil {
		ps = []*memory.Plan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": ps})
}

// --- /v1/plan/versions (history) ---

func (d *Daemon) handlePlanVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing scope")
		return
	}
	slot := q.Get("slot")
	// Optional ?version=N selects a single historical version.
	if vs := q.Get("version"); vs != "" {
		v, err := strconv.ParseInt(vs, 10, 64)
		if err != nil {
			writeErrorStatus(w, http.StatusBadRequest, "bad version")
			return
		}
		p, err := d.store.Memory().GetPlanVersion(r.Context(), scope, slot, v)
		if err != nil {
			if errors.Is(err, rsql.ErrNotFound) {
				writeErrorStatus(w, http.StatusNotFound, "not found")
				return
			}
			writeErrorStatus(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	vs, err := d.store.Memory().ListPlanVersions(r.Context(), scope, slot)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if vs == nil {
		vs = []*memory.Plan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

// --- /v1/learnings ---

func (d *Daemon) handleLearnings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing scope")
		return
	}
	switch r.Method {
	case http.MethodPost:
		d.appendLearning(w, r, scope, q.Get("supersedes"))
	case http.MethodGet:
		inherit := q.Get("inherit") == "true"
		includeSuperseded := q.Get("include") == "superseded"
		d.listLearnings(w, r, scope, inherit, includeSuperseded)
	default:
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (d *Daemon) appendLearning(w http.ResponseWriter, r *http.Request, scope, supersedes string) {
	defer r.Body.Close()
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	id := newLearningID()
	l := &memory.Learning{ID: id, Scope: scope, Content: body.Content, Author: d.localAuthor()}
	if supersedes != "" {
		v := supersedes
		l.SupersedesID = &v
	}
	if err := d.store.Memory().AppendLearning(r.Context(), l); err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	got, err := d.store.Memory().GetLearning(r.Context(), id)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d.sync != nil {
		if err := d.sync.EnqueueLearning(r.Context(), got); err != nil {
			logSyncEnqueueErr("learning", scope+"/"+id, err)
		}
	}
	writeJSON(w, http.StatusCreated, got)
}

func (d *Daemon) listLearnings(w http.ResponseWriter, r *http.Request, scope string, inherit, includeSuperseded bool) {
	var ls []*memory.Learning
	if inherit {
		ls = memory.CollectLearningsChain(r.Context(), d.store.Memory(), scope, includeSuperseded)
	} else {
		var err error
		ls, err = d.store.Memory().ListLearnings(r.Context(), scope, includeSuperseded)
		if err != nil {
			writeErrorStatus(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if ls == nil {
		ls = []*memory.Learning{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"learnings": ls})
}

// --- /v1/context ---

func (d *Daemon) handleContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing scope")
		return
	}
	ctx := memory.BuildContext(r.Context(), d.store.Memory(), scope)
	writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "context": ctx})
}

// --- helpers ---

// newLearningID returns a stamp-style ID for learning rows. ULIDs
// would be nicer but we'd rather not add the dep just for this; the
// current scheme is sortable enough for v1.
func newLearningID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return "L_" + strconv.FormatInt(timeNowNano(), 10) + "_" + hex.EncodeToString(buf[:])
}

// localAuthor returns the author_user_id to stamp on plan/learning
// writes made through the local daemon. The daemon is a single-user
// process with a shared-secret bearer (no per-request identity), so this
// is the configured local identity if one exists, else empty. round-12B:
// the serve side (per-device bearer) stamps the authenticated user; this
// local path is deliberately a stub returning "" until a local-identity
// config lands. Provenance for shared brains is therefore set on the
// serve write path, not here.
func (d *Daemon) localAuthor() string {
	return d.cfg.LocalAuthor
}

// logSyncEnqueueErr is non-fatal: the local store is already committed,
// so the request still returns success. The next write attempt or the
// next reconcile pass will retry.
func logSyncEnqueueErr(resource, ident string, err error) {
	log.Printf("sync enqueue %s %q: %v", resource, ident, err)
}
