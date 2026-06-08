package serve

import (
	"testing"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// TestWhoami_MultiTenantReportsBrainAndPolicy: a registered user probing
// whoami on a multi-tenant server gets multi_tenant=true, their personal
// brain id, and the default server-side policy.
func TestWhoami_MultiTenantReportsBrainAndPolicy(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	_, tok, personal := regUser(t, base, "owner@example.com")

	var who WhoamiResp
	getOK(t, base+"/v2/sync/whoami", tok, &who)

	if !who.MultiTenant {
		t.Fatal("multi_tenant = false, want true")
	}
	if who.BrainID != personal {
		t.Fatalf("brain_id = %q, want personal %q", who.BrainID, personal)
	}
	if who.EncryptionPolicy != string(rsql.EncryptionPolicyServerSide) {
		t.Fatalf("encryption_policy = %q, want server-side", who.EncryptionPolicy)
	}
}

// TestWhoami_SingleTenant: single-tenant mode reports multi_tenant=false
// with empty brain/policy (the daemon then forces shouldSeal=true).
func TestWhoami_SingleTenant(t *testing.T) {
	base, _, _ := newAtRestServer(t, nil) // single-tenant
	_, tok, _ := regUser(t, base, "solo@example.com")

	var who WhoamiResp
	getOK(t, base+"/v2/sync/whoami", tok, &who)

	if who.MultiTenant {
		t.Fatal("multi_tenant = true, want false in single-tenant mode")
	}
	if who.BrainID != "" || who.EncryptionPolicy != "" {
		t.Fatalf("single-tenant whoami leaked brain/policy: %+v", who)
	}
}

// TestWhoami_RequiresAuth: whoami is auth-gated (requireBrain) just like
// push/pull — an unauthenticated probe is rejected.
func TestWhoami_RequiresAuth(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	getStatus(t, base+"/v2/sync/whoami", "", 401)
}

// TestWhoami_HonorsBrainSelector: ?brain=<shared> resolves to the shared
// brain (with its own policy), not the personal one.
func TestWhoami_HonorsBrainSelector(t *testing.T) {
	_, base, _ := newIdentityServer(t)
	_, tok, _ := regUser(t, base, "owner@example.com")

	var created CreateBrainResp
	post(t, base+"/v1/brains", tok, CreateBrainReq{Name: "team"}, &created, 201)

	var who WhoamiResp
	getOK(t, base+"/v2/sync/whoami?brain="+created.BrainID, tok, &who)
	if who.BrainID != created.BrainID {
		t.Fatalf("brain_id = %q, want shared %q", who.BrainID, created.BrainID)
	}
	if who.EncryptionPolicy != string(rsql.EncryptionPolicyServerSide) {
		t.Fatalf("encryption_policy = %q, want server-side", who.EncryptionPolicy)
	}
}

// TestNew_MultiTenantRequiresMasterKey: serve.New rejects a multi-tenant
// config with no master key, but accepts single-tenant without one.
func TestNew_MultiTenantRequiresMasterKey(t *testing.T) {
	mkBackend := func(t *testing.T) *local.Backend {
		t.Helper()
		b, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		return b
	}

	// Multi-tenant + no master key → error.
	if _, err := New(Config{Backend: mkBackend(t), AuthToken: testToken}); err == nil {
		t.Fatal("multi-tenant New accepted an empty master key")
	}

	// Single-tenant + no master key → ok.
	if _, err := New(Config{Backend: mkBackend(t), AuthToken: testToken, SingleTenant: true}); err != nil {
		t.Fatalf("single-tenant New rejected an empty master key: %v", err)
	}

	// Multi-tenant + valid master key → ok.
	if _, err := New(Config{Backend: mkBackend(t), AuthToken: testToken, MasterKey: mustMaster(t)}); err != nil {
		t.Fatalf("multi-tenant New rejected a valid master key: %v", err)
	}
}
