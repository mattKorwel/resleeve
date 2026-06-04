package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/serve"
)

// loginAndUnwrapKEK runs the same challenge → login → unwrap sequence
// as `resleeve login`, but returns the raw KEK bytes (plus the fresh
// device token) instead of poking the daemon for a seal-unlock.
//
// Callers:
//   - `resleeve migrate-key`: needs the KEK to re-encrypt sealed blobs.
//     Passes ttl=0 (long-lived device).
//   - `resleeve pair invite`: needs the KEK to re-wrap under the pair code.
//     Passes ttl=60s — the issued token is revoked client-side moments
//     later; the server-side TTL is the safety net (sec-M5) for the case
//     where the revoke fails (network, ^C). Server clamps to ≤10 min.
//
// deviceName is the cosmetic label sent on /v2/auth/login. migrate-key
// passes the host name (so the row in `devices` is identifiable);
// pair-invite passes "pair-invite-ephemeral".
func loginAndUnwrapKEK(ctx context.Context, upstream, email, password, deviceName string, ttl time.Duration) (auth.KEK, string, error) {
	var chal serve.LoginChallengeResp
	if err := postJSON(ctx, upstream+"/v2/auth/login-challenge", "",
		serve.LoginChallengeReq{Email: email}, &chal); err != nil {
		return auth.KEK{}, "", fmt.Errorf("login-challenge: %w", err)
	}
	params := auth.Argon2idParams{
		MemoryKiB:   chal.Params.MemoryKiB,
		TimeIters:   chal.Params.TimeIters,
		Parallelism: chal.Params.Parallelism,
	}
	verifier := auth.DeriveKey([]byte(password), chal.VerifierSalt, params)
	var resp serve.LoginResp
	if err := postJSON(ctx, upstream+"/v2/auth/login", "",
		serve.LoginReq{
			Email:           email,
			VerifierHash:    verifier,
			Device:          serve.DeviceMetadata{Name: deviceName},
			ExpiresAtHintNS: ttl.Nanoseconds(),
		}, &resp); err != nil {
		return auth.KEK{}, "", fmt.Errorf("login: %w", err)
	}
	wrapped := auth.WrappedKEK{
		Salt: resp.WrappedKEK.Salt, Nonce: resp.WrappedKEK.Nonce, Ciphertext: resp.WrappedKEK.CT,
		Params: auth.Argon2idParams{
			MemoryKiB:   resp.Params.MemoryKiB,
			TimeIters:   resp.Params.TimeIters,
			Parallelism: resp.Params.Parallelism,
		},
	}
	kek, err := wrapped.Unwrap([]byte(password))
	if err != nil {
		return auth.KEK{}, "", fmt.Errorf("unwrap KEK (likely wrong password): %w", err)
	}
	return kek, resp.DeviceToken, nil
}
