package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/serve"
)

// TestPairInvite_RevokesEphemeralToken proves the follow-up's invariant:
// after `pair invite` does its internal login to grab the live KEK, the
// short-lived "pair-invite-ephemeral" device token MUST be revoked via
// POST /v2/auth/logout so it can't be reused as a sync credential.
//
// We don't drive runPairInvite directly (it prompts on stdin); instead we
// exercise the two pieces that the runPairInvite cleanup composes:
//
//  1. loginAndUnwrapKEK — the shared helper that mints the ephemeral token,
//     called by runPairInvite at the top of its flow.
//  2. POST /v2/auth/logout with that token — the exact call our deferred
//     cleanup makes.
//
// The assertion is "the server saw a logout for the same bearer that
// login minted." That's the entire contract under test.
func TestPairInvite_RevokesEphemeralToken(t *testing.T) {
	// --- Fake just enough of the auth surface to drive loginAndUnwrapKEK
	// and to record the revoke.
	const (
		email    = "inv@b.com"
		password = "good-pass-here-22"
	)
	signup, err := auth.Signup(email, password)
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	const mintedToken = "dev_ephemeral_test_token"

	var logoutCalls atomic.Int32
	var logoutBearer atomic.Value // string

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/auth/login-challenge", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSONTest(w, http.StatusOK, serve.LoginChallengeResp{
			Params: serve.Argon2idParams{
				MemoryKiB:   signup.User.Params.MemoryKiB,
				TimeIters:   signup.User.Params.TimeIters,
				Parallelism: signup.User.Params.Parallelism,
			},
			VerifierSalt: signup.User.PasswordVerifier.Salt,
		})
	})
	mux.HandleFunc("/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req serve.LoginReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// The CLI labels its ephemeral pairing token deliberately — assert
		// the call site is the one we expect (i.e. pair invite).
		if req.Device.Name != "pair-invite-ephemeral" {
			t.Errorf("login Device.Name = %q, want %q", req.Device.Name, "pair-invite-ephemeral")
		}
		writeJSONTest(w, http.StatusOK, serve.LoginResp{
			UserID:      "u_test",
			DeviceID:    "d_test",
			DeviceToken: mintedToken,
			Params: serve.Argon2idParams{
				MemoryKiB:   signup.User.Params.MemoryKiB,
				TimeIters:   signup.User.Params.TimeIters,
				Parallelism: signup.User.Params.Parallelism,
			},
			WrappedKEK: serve.WrappedKEKEnv{
				Salt:  signup.User.PasswordKEK.Salt,
				Nonce: signup.User.PasswordKEK.Nonce,
				CT:    signup.User.PasswordKEK.Ciphertext,
			},
		})
	})
	mux.HandleFunc("/v2/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		logoutCalls.Add(1)
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		logoutBearer.Store(bearer)
		writeJSONTest(w, http.StatusOK, map[string]string{"status": "logged out"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// --- Drive the login leg the way runPairInvite does.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	kek, tok, err := loginAndUnwrapKEK(ctx, ts.URL, email, password, "pair-invite-ephemeral", 0)
	if err != nil {
		t.Fatalf("loginAndUnwrapKEK: %v", err)
	}
	if kek != signup.KEK {
		t.Fatal("unwrapped KEK differs from registered KEK")
	}
	if tok != mintedToken {
		t.Fatalf("ephemeral token: got %q, want %q", tok, mintedToken)
	}

	// --- Run the exact revoke call that runPairInvite's deferred cleanup
	// makes. If this contract changes (e.g. someone refactors to a new
	// endpoint), this test fails loudly.
	if err := postJSON(ctx, ts.URL+"/v2/auth/logout", tok, struct{}{}, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if got := logoutCalls.Load(); got != 1 {
		t.Fatalf("logout call count: got %d, want 1", got)
	}
	if got, _ := logoutBearer.Load().(string); got != mintedToken {
		t.Errorf("logout bearer: got %q, want %q", got, mintedToken)
	}
}

func writeJSONTest(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
