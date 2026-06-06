package serve

import (
	"context"
	"testing"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// TestRegister_ProvisionsPersonalBrain asserts the round-11 multi-tenant
// foundation: registering a user auto-creates exactly one personal brain
// owned by that user plus a membership of it. The brain id is returned in
// the register response, IsMember reports the edge, and ListBrains(userID)
// returns that brain.
func TestRegister_ProvisionsPersonalBrain(t *testing.T) {
	_, base, store := newIdentityServer(t)
	ctx := context.Background()

	def := paramsToWire(auth.DefaultArgon2idParams())
	var resp RegisterResp
	post(t, base+"/v2/auth/register", "", buildRegisterReq(t, "solo@example.com", def), &resp, 201)

	if resp.UserID == "" {
		t.Fatal("register: empty user_id")
	}
	if resp.BrainID == "" {
		t.Fatal("register: empty brain_id — personal brain not provisioned")
	}

	// Exactly one brain, owned by the new user, kind=personal.
	owned, err := store.Brains().ListByOwner(ctx, resp.UserID)
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(owned) != 1 {
		t.Fatalf("ListByOwner = %d brains, want exactly 1", len(owned))
	}
	b := owned[0]
	if b.ID != resp.BrainID {
		t.Errorf("owned brain id %q != response brain_id %q", b.ID, resp.BrainID)
	}
	if b.Kind != rsql.BrainKindPersonal {
		t.Errorf("brain kind = %q, want personal", b.Kind)
	}
	if b.OwnerUserID != resp.UserID {
		t.Errorf("brain owner = %q, want %q", b.OwnerUserID, resp.UserID)
	}

	// Exactly one membership: the user is a member of their personal brain.
	ok, err := store.Memberships().IsMember(ctx, resp.UserID, resp.BrainID)
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if !ok {
		t.Error("IsMember(user, personal brain) = false, want true")
	}

	brains, err := store.Memberships().ListBrains(ctx, resp.UserID)
	if err != nil {
		t.Fatalf("ListBrains: %v", err)
	}
	if len(brains) != 1 || brains[0] != resp.BrainID {
		t.Errorf("ListBrains(user) = %v, want [%s]", brains, resp.BrainID)
	}

	members, err := store.Memberships().ListMembers(ctx, resp.BrainID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 || members[0] != resp.UserID {
		t.Errorf("ListMembers(brain) = %v, want [%s]", members, resp.UserID)
	}
}

// TestRegister_DistinctBrainsPerUser asserts two registrations get two
// distinct personal brains and don't see each other's membership.
func TestRegister_DistinctBrainsPerUser(t *testing.T) {
	_, base, store := newIdentityServer(t)
	ctx := context.Background()
	def := paramsToWire(auth.DefaultArgon2idParams())

	var a, b RegisterResp
	post(t, base+"/v2/auth/register", "", buildRegisterReq(t, "a@example.com", def), &a, 201)
	post(t, base+"/v2/auth/register", "", buildRegisterReq(t, "b@example.com", def), &b, 201)

	if a.BrainID == b.BrainID {
		t.Fatalf("both users share brain id %q", a.BrainID)
	}
	if ok, _ := store.Memberships().IsMember(ctx, a.UserID, b.BrainID); ok {
		t.Error("user a is unexpectedly a member of user b's personal brain")
	}
	if ok, _ := store.Memberships().IsMember(ctx, b.UserID, a.BrainID); ok {
		t.Error("user b is unexpectedly a member of user a's personal brain")
	}
}
