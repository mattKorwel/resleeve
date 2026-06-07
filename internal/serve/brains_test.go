package serve

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
)

// regUser registers a fresh user and returns (userID, deviceToken,
// personalBrainID). Uses the same wire path the CLI does.
func regUser(t *testing.T, base, email string) (userID, token, brainID string) {
	t.Helper()
	def := paramsToWire(auth.DefaultArgon2idParams())
	var resp RegisterResp
	post(t, base+"/v2/auth/register", "", buildRegisterReq(t, email, def), &resp, 201)
	if resp.UserID == "" || resp.DeviceToken == "" {
		t.Fatalf("register %s: empty user/token", email)
	}
	return resp.UserID, resp.DeviceToken, resp.BrainID
}

// TestBrain_CreateAndList: a user creates a shared brain, it shows up in
// their brain list with role=owner alongside their personal brain.
func TestBrain_CreateAndList(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	_, tok, personal := regUser(t, base, "owner@example.com")

	var created CreateBrainResp
	post(t, base+"/v1/brains", tok, CreateBrainReq{Name: "team"}, &created, 201)
	if created.BrainID == "" {
		t.Fatal("create brain: empty id")
	}

	var list ListBrainsResp
	getOK(t, base+"/v1/brains", tok, &list)
	roles := map[string]string{}
	for _, b := range list.Brains {
		roles[b.ID] = b.Role
	}
	if roles[personal] != "owner" {
		t.Errorf("personal brain role = %q, want owner", roles[personal])
	}
	if roles[created.BrainID] != "owner" {
		t.Errorf("created brain role = %q, want owner", roles[created.BrainID])
	}
}

// TestBrain_CreateRejectsLegacyBearer: management endpoints require a
// real per-device user, never the legacy single bearer.
func TestBrain_CreateRejectsLegacyBearer(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	postStatus(t, base+"/v1/brains", testToken, CreateBrainReq{Name: "x"}, 401)
}

// TestBrain_MembershipAuthzMatrix exercises the owner / member /
// non-member authz rules for each management verb.
func TestBrain_MembershipAuthzMatrix(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	ownerID, ownerTok, _ := regUser(t, base, "owner@example.com")
	memberID, memberTok, _ := regUser(t, base, "member@example.com")
	_, strangerTok, _ := regUser(t, base, "stranger@example.com")

	var created CreateBrainResp
	post(t, base+"/v1/brains", ownerTok, CreateBrainReq{Name: "team"}, &created, 201)
	brain := created.BrainID
	membersURL := base + "/v1/brains/" + brain + "/members"

	// Owner can list members; sees themselves.
	var lm ListMembersResp
	getOK(t, membersURL, ownerTok, &lm)
	if len(lm.Members) != 1 || lm.Members[0] != ownerID {
		t.Fatalf("initial members = %v, want [%s]", lm.Members, ownerID)
	}

	// Non-member CANNOT list (403).
	getStatus(t, membersURL, strangerTok, 403)
	// Non-member CANNOT add (403).
	postStatus(t, membersURL, strangerTok, AddMemberReq{UserID: memberID}, 403)

	// Member is added by the owner (owner-only succeeds, 204).
	postStatus(t, membersURL, ownerTok, AddMemberReq{UserID: memberID}, 204)

	// Now the member can list (200) but is NOT the owner → cannot add.
	getOK(t, membersURL, memberTok, &lm)
	if len(lm.Members) != 2 {
		t.Errorf("after add, members = %v, want 2", lm.Members)
	}
	// A non-owner member cannot add another member.
	_, _, _ = regUser(t, base, "fourth@example.com")
	postStatus(t, membersURL, memberTok, AddMemberReq{UserID: "fourth"}, 403)

	// Non-owner cannot remove.
	deleteStatus(t, membersURL+"/"+memberID, memberTok, 403)

	// Owner-cannot-orphan: removing the owner is a 400.
	deleteStatus(t, membersURL+"/"+ownerID, ownerTok, 400)

	// Owner removes the member (204), then the (now-)non-member 403s on list.
	deleteStatus(t, membersURL+"/"+memberID, ownerTok, 204)
	getStatus(t, membersURL, memberTok, 403)
}

// TestBrain_AddUnknownUser rejects a typo'd user id with 400.
func TestBrain_AddUnknownUser(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	_, ownerTok, _ := regUser(t, base, "owner@example.com")
	var created CreateBrainResp
	post(t, base+"/v1/brains", ownerTok, CreateBrainReq{Name: "team"}, &created, 201)
	postStatus(t, base+"/v1/brains/"+created.BrainID+"/members", ownerTok,
		AddMemberReq{UserID: "nope-not-a-real-user"}, 400)
}

// TestBrain_NonMemberManageUnknownBrain: an owner managing a brain that
// doesn't exist gets 404; a stranger gets 404 too (Get fails before the
// owner check). The point is no information leak about real brains.
func TestBrain_ManageUnknownBrain(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	_, tok, _ := regUser(t, base, "owner@example.com")
	postStatus(t, base+"/v1/brains/deadbeef/members", tok, AddMemberReq{UserID: "x"}, 404)
}

// TestBrain_SharedKeyspaceRoundTrip is the end-to-end sharing assertion:
// owner pushes into the shared brain (?brain=<id>); a member added to that
// brain can pull the same row (?brain=<id>); a non-member is 403'd on the
// selector; and the personal keyspace stays isolated (pulling the
// personal brain does NOT see the shared row).
func TestBrain_SharedKeyspaceRoundTrip(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	_, ownerTok, ownerPersonal := regUser(t, base, "owner@example.com")
	memberID, memberTok, _ := regUser(t, base, "member@example.com")
	_, strangerTok, _ := regUser(t, base, "stranger@example.com")

	var created CreateBrainResp
	post(t, base+"/v1/brains", ownerTok, CreateBrainReq{Name: "team"}, &created, 201)
	brain := created.BrainID
	postStatus(t, base+"/v1/brains/"+brain+"/members", ownerTok, AddMemberReq{UserID: memberID}, 204)

	// Owner pushes a memory row into the SHARED brain.
	push := PushReq{Batch: []PushRow{{Key: "memory/team-scope", Blob: []byte("shared-blob")}}}
	postStatus(t, base+"/v2/sync/push?brain="+brain, ownerTok, push, 200)

	// Member pulls the shared brain and sees the row, with the
	// brain-agnostic key (no <brain>/ prefix leaked to the client).
	var pulled PullResp
	getOK(t, base+"/v2/sync/pull?kind=memory&brain="+brain, memberTok, &pulled)
	if len(pulled.Rows) != 1 {
		t.Fatalf("member pull shared: got %d rows, want 1", len(pulled.Rows))
	}
	if pulled.Rows[0].Key != "memory/team-scope" {
		t.Errorf("pulled key = %q, want brain-agnostic memory/team-scope", pulled.Rows[0].Key)
	}
	if string(pulled.Rows[0].Blob) != "shared-blob" {
		t.Errorf("pulled blob = %q, want shared-blob", pulled.Rows[0].Blob)
	}

	// Stranger is forbidden from the selector on both push and pull.
	postStatus(t, base+"/v2/sync/push?brain="+brain, strangerTok, push, 403)
	getStatus(t, base+"/v2/sync/pull?kind=memory&brain="+brain, strangerTok, 403)

	// Isolation: pulling the OWNER's personal brain (default selector)
	// must NOT surface the shared row.
	var personalPull PullResp
	getOK(t, base+"/v2/sync/pull?kind=memory", ownerTok, &personalPull)
	for _, r := range personalPull.Rows {
		if r.Key == "memory/team-scope" {
			t.Errorf("shared row leaked into personal brain %s", ownerPersonal)
		}
	}
}

// --- GET / DELETE test helpers (post/postStatus only cover POST) ---

func getOK(t *testing.T, url, bearer string, out any) {
	t.Helper()
	getReq(t, url, bearer, out, 200)
}

func getStatus(t *testing.T, url, bearer string, want int) {
	t.Helper()
	getReq(t, url, bearer, nil, want)
}

func getReq(t *testing.T, url, bearer string, out any, want int) {
	t.Helper()
	doReq(t, http.MethodGet, url, bearer, out, want)
}

func deleteStatus(t *testing.T, url, bearer string, want int) {
	t.Helper()
	doReq(t, http.MethodDelete, url, bearer, nil, want)
}

func doReq(t *testing.T, method, url, bearer string, out any, want int) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new %s req: %v", method, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: got %d, want %d (%s)", method, url, resp.StatusCode, want, readAll(t, resp.Body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
}
