package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	mux.HandleFunc("/v1/plans", d.requireBearer(d.handlePlans))
	mux.HandleFunc("/v1/learnings", d.requireBearer(d.handleLearnings))
	mux.HandleFunc("/v1/context", d.requireBearer(d.handleContext))
}

// --- /v1/scope ---

func (d *Daemon) handleScope(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, http.ErrBodyNotAllowed) {
		// Empty body is OK; surface other decode errors.
		if err.Error() != "EOF" {
			http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	kind := memory.ScopeKind(body.Kind)
	if !kind.Valid() {
		http.Error(w, "invalid kind", http.StatusBadRequest)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	got, err := d.store.Memory().GetScope(r.Context(), path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (d *Daemon) deleteScope(w http.ResponseWriter, r *http.Request, path string) {
	if err := d.store.Memory().DeleteScope(r.Context(), path); err != nil {
		if errors.Is(err, memory.ErrScopeHasChildren) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- /v1/scopes ---

func (d *Daemon) handleScopes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scopes, err := d.store.Memory().ListScopes(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "missing scope", http.StatusBadRequest)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *Daemon) putPlan(w http.ResponseWriter, r *http.Request, scope, slot string) {
	defer r.Body.Close()
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	p := &memory.Plan{Scope: scope, Name: slot, Content: body.Content}
	if err := d.store.Memory().PutPlan(r.Context(), p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	got, err := d.store.Memory().GetPlan(r.Context(), scope, slot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		http.Error(w, "missing scope", http.StatusBadRequest)
		return
	}
	ps, err := d.store.Memory().ListPlans(r.Context(), scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ps == nil {
		ps = []*memory.Plan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": ps})
}

// --- /v1/learnings ---

func (d *Daemon) handleLearnings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		http.Error(w, "missing scope", http.StatusBadRequest)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *Daemon) appendLearning(w http.ResponseWriter, r *http.Request, scope, supersedes string) {
	defer r.Body.Close()
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := newLearningID()
	l := &memory.Learning{ID: id, Scope: scope, Content: body.Content}
	if supersedes != "" {
		v := supersedes
		l.SupersedesID = &v
	}
	if err := d.store.Memory().AppendLearning(r.Context(), l); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	got, err := d.store.Memory().GetLearning(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		http.Error(w, "missing scope", http.StatusBadRequest)
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

// logSyncEnqueueErr is non-fatal: the local store is already committed,
// so the request still returns success. The next write attempt or the
// next reconcile pass will retry.
func logSyncEnqueueErr(resource, ident string, err error) {
	log.Printf("sync enqueue %s %q: %v", resource, ident, err)
}
