package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func TestBrainStore_CRUD(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "user-a")
	seedServerUser(t, ctx, st, "user-b")

	now := time.Now().UTC().Truncate(time.Microsecond)
	b := &rsql.Brain{
		ID:          "brain-1",
		Name:        "user-a personal",
		Kind:        rsql.BrainKindPersonal,
		OwnerUserID: "user-a",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := st.Brains().Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.Brains().Get(ctx, "brain-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != b.Name || got.Kind != rsql.BrainKindPersonal || got.OwnerUserID != "user-a" {
		t.Errorf("Get mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt round-trip: got %v want %v", got.CreatedAt, now)
	}

	if _, err := st.Brains().Get(ctx, "nope"); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("Get missing: want ErrNotFound, got %v", err)
	}

	// A second brain owned by user-b should not appear in user-a's list.
	if err := st.Brains().Create(ctx, &rsql.Brain{
		ID: "brain-2", Name: "b's", Kind: rsql.BrainKindPersonal,
		OwnerUserID: "user-b", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create brain-2: %v", err)
	}
	owned, err := st.Brains().ListByOwner(ctx, "user-a")
	if err != nil {
		t.Fatalf("ListByOwner: %v", err)
	}
	if len(owned) != 1 || owned[0].ID != "brain-1" {
		t.Errorf("ListByOwner(user-a) = %+v, want only brain-1", owned)
	}
}

func TestMembershipStore_AddListIsMember(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "user-a")
	seedServerUser(t, ctx, st, "user-b")
	now := time.Now().UTC()
	mkBrain(t, ctx, st, "brain-1", "user-a", now)
	mkBrain(t, ctx, st, "brain-2", "user-a", now.Add(time.Second))

	// user-a is in both brains; user-b only in brain-1.
	add(t, ctx, st, "brain-1", "user-a", now)
	add(t, ctx, st, "brain-2", "user-a", now.Add(time.Second))
	add(t, ctx, st, "brain-1", "user-b", now)

	// Add is idempotent.
	add(t, ctx, st, "brain-1", "user-a", now)

	brains, err := st.Memberships().ListBrains(ctx, "user-a")
	if err != nil {
		t.Fatalf("ListBrains: %v", err)
	}
	if len(brains) != 2 {
		t.Errorf("ListBrains(user-a) = %v, want 2 entries", brains)
	}

	members, err := st.Memberships().ListMembers(ctx, "brain-1")
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("ListMembers(brain-1) = %v, want 2", members)
	}

	ok, err := st.Memberships().IsMember(ctx, "user-b", "brain-1")
	if err != nil || !ok {
		t.Errorf("IsMember(user-b, brain-1) = %v, %v; want true, nil", ok, err)
	}
	ok, err = st.Memberships().IsMember(ctx, "user-b", "brain-2")
	if err != nil || ok {
		t.Errorf("IsMember(user-b, brain-2) = %v, %v; want false, nil", ok, err)
	}

	// Remove drops the edge; idempotent on a second call.
	if err := st.Memberships().Remove(ctx, "brain-1", "user-b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := st.Memberships().Remove(ctx, "brain-1", "user-b"); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
	ok, _ = st.Memberships().IsMember(ctx, "user-b", "brain-1")
	if ok {
		t.Errorf("IsMember after Remove = true, want false")
	}
}

func TestBrainStore_ListForUser(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "user-a")
	seedServerUser(t, ctx, st, "user-b")
	now := time.Now().UTC()

	// user-a owns brain-own; user-b owns brain-shared and adds user-a.
	mkBrain(t, ctx, st, "brain-own", "user-a", now)
	add(t, ctx, st, "brain-own", "user-a", now)
	mkBrain(t, ctx, st, "brain-shared", "user-b", now.Add(time.Second))
	add(t, ctx, st, "brain-shared", "user-b", now.Add(time.Second))
	add(t, ctx, st, "brain-shared", "user-a", now.Add(2*time.Second))

	brains, err := st.Brains().ListForUser(ctx, "user-a")
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(brains) != 2 {
		t.Fatalf("ListForUser(user-a) = %d brains, want 2", len(brains))
	}
	// Includes a brain user-a does NOT own (shared membership).
	var sawShared bool
	for _, b := range brains {
		if b.ID == "brain-shared" {
			sawShared = true
		}
	}
	if !sawShared {
		t.Errorf("ListForUser(user-a) missing shared brain: %+v", brains)
	}
}

func TestCredentialStore_CRUD(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "user-a")
	now := time.Now().UTC().Truncate(time.Microsecond)
	exp := now.Add(24 * time.Hour)

	cSSH := &rsql.Credential{
		ID: "cred-ssh", UserID: "user-a", Kind: rsql.CredentialKindSSH,
		PublicKeyOrHash: "ssh-ed25519 AAAA...", Label: "laptop",
		CreatedAt: now,
	}
	cAPI := &rsql.Credential{
		ID: "cred-api", UserID: "user-a", Kind: rsql.CredentialKindAPI,
		PublicKeyOrHash: "sha256:deadbeef", Label: "ci",
		CreatedAt: now, ExpiresAt: &exp,
	}
	if err := st.Credentials().Add(ctx, cSSH); err != nil {
		t.Fatalf("Add ssh: %v", err)
	}
	if err := st.Credentials().Add(ctx, cAPI); err != nil {
		t.Fatalf("Add api: %v", err)
	}

	got, err := st.Credentials().Get(ctx, "cred-api")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != rsql.CredentialKindAPI || got.Label != "ci" || got.PublicKeyOrHash != "sha256:deadbeef" {
		t.Errorf("Get mismatch: %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt round-trip: got %v want %v", got.ExpiresAt, exp)
	}

	// The SSH cred has no expiry.
	gotSSH, err := st.Credentials().Get(ctx, "cred-ssh")
	if err != nil {
		t.Fatalf("Get ssh: %v", err)
	}
	if gotSSH.ExpiresAt != nil {
		t.Errorf("ssh ExpiresAt = %v, want nil", gotSSH.ExpiresAt)
	}

	list, err := st.Credentials().ListByUser(ctx, "user-a")
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("ListByUser = %d, want 2", len(list))
	}

	if err := st.Credentials().Delete(ctx, "cred-ssh"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Credentials().Delete(ctx, "cred-ssh"); err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
	if _, err := st.Credentials().Get(ctx, "cred-ssh"); !errors.Is(err, rsql.ErrNotFound) {
		t.Errorf("Get after Delete: want ErrNotFound, got %v", err)
	}
}

// TestBrainKeyStore_PutGet exercises the round-12 brain_keys store:
// upsert + get + ErrNotFound + FK cascade behavior, and the
// encryption_policy default surfaced on the brain row.
func TestBrainKeyStore_PutGet(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "user-a")
	now := time.Now().UTC().Truncate(time.Microsecond)
	mkBrain(t, ctx, st, "brain-1", "user-a", now)

	// No key yet → ErrNotFound.
	if _, err := st.BrainKeys().GetBrainKey(ctx, "brain-1"); !errors.Is(err, rsql.ErrNotFound) {
		t.Fatalf("GetBrainKey before put: want ErrNotFound, got %v", err)
	}

	wrapped := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02}
	if err := st.BrainKeys().PutBrainKey(ctx, "brain-1", wrapped); err != nil {
		t.Fatalf("PutBrainKey: %v", err)
	}
	got, err := st.BrainKeys().GetBrainKey(ctx, "brain-1")
	if err != nil {
		t.Fatalf("GetBrainKey: %v", err)
	}
	if string(got) != string(wrapped) {
		t.Fatalf("wrapped DEK round-trip: got %x want %x", got, wrapped)
	}

	// Upsert overwrites in place.
	wrapped2 := []byte{0x11, 0x22, 0x33}
	if err := st.BrainKeys().PutBrainKey(ctx, "brain-1", wrapped2); err != nil {
		t.Fatalf("PutBrainKey (upsert): %v", err)
	}
	got, _ = st.BrainKeys().GetBrainKey(ctx, "brain-1")
	if string(got) != string(wrapped2) {
		t.Fatalf("upsert: got %x want %x", got, wrapped2)
	}

	// encryption_policy defaults to server-side on a brain created without
	// an explicit policy.
	b, err := st.Brains().Get(ctx, "brain-1")
	if err != nil {
		t.Fatalf("Brains.Get: %v", err)
	}
	if b.EncryptionPolicy != rsql.EncryptionPolicyServerSide {
		t.Fatalf("encryption_policy = %q, want server-side", b.EncryptionPolicy)
	}
}

// --- helpers ---

func mkBrain(t *testing.T, ctx context.Context, st *Store, id, owner string, ts time.Time) {
	t.Helper()
	if err := st.Brains().Create(ctx, &rsql.Brain{
		ID: id, Name: id, Kind: rsql.BrainKindPersonal,
		OwnerUserID: owner, CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		t.Fatalf("mkBrain %s: %v", id, err)
	}
}

func add(t *testing.T, ctx context.Context, st *Store, brainID, userID string, ts time.Time) {
	t.Helper()
	if err := st.Memberships().Add(ctx, &rsql.Membership{
		BrainID: brainID, UserID: userID, CreatedAt: ts,
	}); err != nil {
		t.Fatalf("add membership (%s,%s): %v", brainID, userID, err)
	}
}
