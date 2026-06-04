package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// seedServerUser inserts a minimal server_users row so pairing_codes
// FK constraints to user_id are satisfied. The verifier/KEK fields
// hold placeholder bytes — we're testing the SQL surface, not the
// crypto.
func seedServerUser(t *testing.T, ctx context.Context, st *Store, id string) {
	t.Helper()
	u := &rsql.ServerUser{
		ID:        id,
		Email:     id + "@example.test",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Params:    auth.Argon2idParams{MemoryKiB: 1, TimeIters: 1, Parallelism: 1},
		PasswordVerifier: auth.Verifier{Salt: []byte("s"), Hash: []byte("h")},
		PasswordKEK: auth.WrappedKEK{
			Salt: []byte("ks"), Nonce: []byte("kn"), Ciphertext: []byte("kc"),
			Params: auth.Argon2idParams{MemoryKiB: 1, TimeIters: 1, Parallelism: 1},
		},
		RecoveryVerifier: auth.Verifier{Salt: []byte("rs"), Hash: []byte("rh")},
		RecoveryKEK: auth.WrappedKEK{
			Salt: []byte("rks"), Nonce: []byte("rkn"), Ciphertext: []byte("rkc"),
			Params: auth.Argon2idParams{MemoryKiB: 1, TimeIters: 1, Parallelism: 1},
		},
	}
	if err := st.ServerUsers().Create(ctx, u); err != nil {
		t.Fatalf("seed server user %s: %v", id, err)
	}
}

// newPairingCode returns a PairingCode shell with the cryptographic
// fields zeroed — enough to exercise the SQL surface (Create / Get /
// Claim / SweepExpired) without dragging in real Argon2id derivation
// per row, which would dominate the test runtime.
func newPairingCode(id, userID string, created, expires time.Time) *rsql.PairingCode {
	return &rsql.PairingCode{
		CodeID:   id,
		UserID:   userID,
		Params:   auth.Argon2idParams{MemoryKiB: 1, TimeIters: 1, Parallelism: 1},
		Verifier: auth.Verifier{Salt: []byte("salt"), Hash: []byte("hash")},
		WrappedK: auth.WrappedKEK{
			Salt: []byte("kek-salt"), Nonce: []byte("nonce"), Ciphertext: []byte("ct"),
			Params: auth.Argon2idParams{MemoryKiB: 1, TimeIters: 1, Parallelism: 1},
		},
		CreatedAt: created,
		ExpiresAt: expires,
	}
}

// TestPairing_ClaimLexOrderMatchesTimeOrder is the regression test
// for Q17 on pairing_codes.expires_at — the column is lexicographically
// compared by Claim (`WHERE expires_at > ?`) and SweepExpired
// (`WHERE expires_at <= ?`). Under time.RFC3339Nano trailing-zero
// stripping, an unexpired code's formatted expires_at can lex-sort
// BELOW a `now` probe formatted with more frac digits, causing Claim
// to silently 404 a valid code (or SweepExpired to garbage-collect it).
// The fix is the fixed-width pairingTimeLayout in identity.go.
//
// Iterates 60 times: a generous oversample of G's ~0.3% per-pair
// flake rate on macOS.
func TestPairing_ClaimLexOrderMatchesTimeOrder(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "U")

	const N = 60
	for i := 0; i < N; i++ {
		// Two adjacent time.Now() snapshots, with the code's expiry set
		// strictly AFTER the claim probe. Any correct
		// `WHERE expires_at > now` test MUST keep the row eligible.
		now := time.Now().UTC()
		expires := now.Add(10 * time.Minute)

		code := newPairingCode("C"+itoa(i), "U", now, expires)
		if err := st.Pairings().Create(ctx, code); err != nil {
			t.Fatalf("iter %d: create: %v", i, err)
		}

		// The just-inserted code MUST claim cleanly — it's strictly
		// unexpired by 10 minutes in wall time. A lex-order bug would
		// surface as ErrNotFound here.
		if err := st.Pairings().Claim(ctx, code.CodeID, now); err != nil {
			t.Errorf("iter %d: Claim returned %v — expected nil (lex-order bug?). now=%s expires=%s",
				i, err,
				now.Format(time.RFC3339Nano),
				expires.Format(time.RFC3339Nano))
		}
	}
}

// TestPairing_SweepExpiredLexOrderMatchesTimeOrder is the cousin
// regression test for the SweepExpired path (`WHERE expires_at <= ?`).
// Asserts that an unexpired code (expires > now) is NEVER swept,
// across many adjacent time.Now() pairs.
func TestPairing_SweepExpiredLexOrderMatchesTimeOrder(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "U")

	const N = 60
	for i := 0; i < N; i++ {
		now := time.Now().UTC()
		// Expires strictly after `now` — sweep MUST NOT delete this row.
		expires := now.Add(10 * time.Minute)

		code := newPairingCode("C"+itoa(i), "U", now, expires)
		if err := st.Pairings().Create(ctx, code); err != nil {
			t.Fatalf("iter %d: create: %v", i, err)
		}
		if err := st.Pairings().SweepExpired(ctx, now); err != nil {
			t.Fatalf("iter %d: sweep: %v", i, err)
		}
		// Row must survive the sweep.
		if _, err := st.Pairings().Get(ctx, code.CodeID); err != nil {
			t.Errorf("iter %d: row vanished after SweepExpired now=%s expires=%s: %v (lex-order bug?)",
				i,
				now.Format(time.RFC3339Nano),
				expires.Format(time.RFC3339Nano),
				err)
		}
		// Cleanup so the next iteration's Create doesn't collide on PK.
		_ = st.Pairings().Delete(ctx, code.CodeID)
	}
}

// TestPairing_LegacyRFC3339NanoStillParses asserts that a row written
// by an older daemon binary (raw RFC3339Nano) still round-trips
// cleanly through the parse-either path. Guards against accidentally
// regressing the parse fallback.
func TestPairing_LegacyRFC3339NanoStillParses(t *testing.T) {
	ctx := context.Background()
	st := openTestStoreFile(t)
	seedServerUser(t, ctx, st, "ULEG")

	// Insert a row by hand using legacy RFC3339Nano formatting — bypass
	// the store's Create so we exercise the parse path against the
	// older-format wire shape.
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(5 * time.Minute)
	_, err := st.db.ExecContext(ctx,
		`INSERT INTO pairing_codes (`+pairingCols+`)
		 VALUES (?,?, ?,?,?, ?,?, ?,?,?, ?, ?, NULL)`,
		"LEGACY", "ULEG",
		uint32(1), uint32(1), uint8(1),
		[]byte("s"), []byte("h"),
		[]byte("ks"), []byte("kn"), []byte("kc"),
		now.Format(time.RFC3339Nano),
		expires.Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	got, err := st.Pairings().Get(ctx, "LEGACY")
	if err != nil {
		t.Fatalf("get legacy: %v", err)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("legacy CreatedAt: got %v, want %v", got.CreatedAt, now)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("legacy ExpiresAt: got %v, want %v", got.ExpiresAt, expires)
	}
}
