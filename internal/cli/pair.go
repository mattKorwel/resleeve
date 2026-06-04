package cli

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/serve"
)

// runPair dispatches the `pair invite` and `pair accept` subcommands.
//
// Pairing flow per docs/design/round-4/02 §"Pairing flow":
//
//	[Device A — inviter, already logged in]
//	  1. resleeve pair invite
//	  2. CLI generates a fresh 30-bit pair code (~6 base32 chars)
//	  3. CLI fetches the daemon's current KEK via /v1/seal/status (sealed
//	     check) + a new /v1/seal/export endpoint (added below) so it can
//	     wrap the KEK under Argon2id(pair_code).
//	  4. POST /v2/auth/pair/publish with verifier + wrapped KEK. Server
//	     returns a code_id (the public half).
//	  5. Inviter prints (code_id, pair_code) for the accepter — these
//	     get shared over an out-of-band channel for ~5 minutes.
//
//	[Device B — accepter, fresh install]
//	  1. resleeve pair accept --code-id=... <pair_code>
//	  2. Derive verifier hash from pair_code + salt (salt is NOT yet known
//	     to B — B asks the server for it via /v2/auth/pair/claim, which
//	     enforces ConstantTimeCompare on the verifier_hash.) [v1 ships
//	     the salt with the published code envelope; the server's verifier
//	     check still gates the wrapped-KEK release.]
//	  3. Server returns device_token + wrapped KEK.
//	  4. CLI unwraps KEK locally using Argon2id(pair_code).
//	  5. CLI stashes the device token in the keychain + POSTs to
//	     /v1/seal/unlock to install the KEK in the local daemon.
func runPair(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve pair <invite|accept> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "invite":
		return runPairInvite(ctx, rest)
	case "accept":
		return runPairAccept(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve pair: unknown subcommand %q\n", sub)
		return 2
	}
}

// runPairInvite generates a fresh pair code, wraps the live KEK under
// it, publishes the envelope to the upstream server, and prints the
// (code_id, pair_code) for the operator to relay to the new device.
func runPairInvite(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("pair invite", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	emailFlag := fs.String("email", "", "account to invite under (default: prompt)")
	ttl := fs.Duration("ttl", 5*time.Minute, "code TTL (server caps at 10 min)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*upstream = pickUpstream(*upstream)
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "pair invite: --upstream required (or set $RESLEEVE_UPSTREAM)")
		return 2
	}
	email := *emailFlag
	if email == "" {
		got, err := promptLine("email: ")
		if err != nil {
			return 1
		}
		email = got
	}
	email = strings.ToLower(strings.TrimSpace(email))

	// Need the master password to fetch the wrapped KEK + unwrap it for
	// re-wrap under the pair code. The daemon doesn't expose its raw
	// KEK over loopback (security: the export route would be a huge
	// attack surface if a hostile local process grabbed it). Re-derive
	// here from email + password via the same login-challenge flow.
	pw, err := promptPassword("master password: ")
	if err != nil {
		return 1
	}

	// 1) login-challenge → 2) derive verifier → 3) login → 4) unwrap KEK.
	// The login call mints a "pair-invite-ephemeral" device token we'll
	// never use for sync; revoke it on the way out (success OR error)
	// so it doesn't linger as a usable bearer credential. Best-effort —
	// a network failure on the revoke shouldn't mask a successful publish.
	kek, ephemeralTok, err := loginAndUnwrapKEK(ctx, *upstream, email, pw, "pair-invite-ephemeral")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair invite:", err)
		return 1
	}
	defer func() {
		if ephemeralTok == "" {
			return
		}
		// Use a fresh context: if the parent ctx was canceled (e.g. user
		// hit ^C after publish), we still want a brief window to revoke.
		revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := postJSON(revokeCtx, *upstream+"/v2/auth/logout", ephemeralTok, struct{}{}, nil); err != nil {
			fmt.Fprintln(os.Stderr, "pair invite: revoke ephemeral token failed (continuing):", err)
		}
	}()

	// 5) Generate a fresh pair code. 60 bits ≈ 12 base32 chars; we
	// chunk as XXXX-XXXX-XXXX for readability. Brief says "60s
	// one-time code" — TTL is server-controlled; we ship 5min default
	// per round-4/02 §"Pairing flow".
	codeBytes := make([]byte, 8)
	if _, err := rand.Read(codeBytes); err != nil {
		fmt.Fprintln(os.Stderr, "pair invite: rand:", err)
		return 1
	}
	pairCode := formatPairCode(codeBytes)

	// 6) Build verifier + wrap the KEK under the pair code. The
	// verifier salt is DETERMINISTIC over the code_id so the accepter
	// (who knows the code_id, not the inviter's random salt) can
	// recompute the same verifier_hash. The KEK-wrap salt stays random
	// (and is returned to the accepter via PairClaimResp.Wrapped.Salt
	// after the verifier check passes). Salts independent per
	// round-2/10 design.
	//
	// We allocate the code_id locally before posting so the deterministic
	// salt is bound to it. The server accepts whatever code_id we send
	// (it's just a public string keyed in the pairing_codes table).
	codeID := newPublicID(8)
	params := auth.Argon2idParams{MemoryKiB: 16 * 1024, TimeIters: 2, Parallelism: 1}
	verifierSalt := auth.PairDeterministicSalt(codeID, auth.PairVerifierLabel)
	verifierHash := auth.DeriveKey([]byte(pairCode), verifierSalt, params)
	wrapped, err := kek.Wrap([]byte(pairCode), params)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair invite: wrap:", err)
		return 1
	}

	// 7) Publish. The inviter's device token authenticates this call.
	kc, err := defaultKeychain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair invite: keychain:", err)
		return 1
	}
	tok, err := kc.Get(*upstream, email)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair invite: keychain get:", err)
		return 1
	}
	if tok == "" {
		fmt.Fprintln(os.Stderr, "pair invite: no device token on file — run `resleeve login` first")
		return 1
	}

	var resp serve.PairPublishResp
	if err := postJSON(ctx, *upstream+"/v2/auth/pair/publish", tok, serve.PairPublishReq{
		CodeID: codeID,
		Params: serve.Argon2idParams{
			MemoryKiB: params.MemoryKiB, TimeIters: params.TimeIters, Parallelism: params.Parallelism,
		},
		Verifier: serve.VerifierEnv{Salt: verifierSalt, Hash: verifierHash},
		Wrapped:  serve.WrappedKEKEnv{Salt: wrapped.Salt, Nonce: wrapped.Nonce, CT: wrapped.Ciphertext},
		TTL:      *ttl,
	}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "pair invite:", err)
		return 1
	}

	fmt.Println("pair code published ✓")
	fmt.Println("share these with the device being onboarded — they expire in ~5 minutes:")
	fmt.Println()
	fmt.Printf("  code id     %s\n", resp.CodeID)
	fmt.Printf("  pair code   %s\n", pairCode)
	fmt.Printf("  expires     %s\n", resp.ExpiresAt.Format(time.RFC3339))
	fmt.Println()
	fmt.Printf("on the new device: resleeve pair accept --code-id=%s --upstream=%s\n", resp.CodeID, *upstream)
	return 0
}

// runPairAccept claims a published pair envelope using the typed pair
// code, unwraps the KEK locally, persists the device token, and
// unlocks the local daemon.
func runPairAccept(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("pair accept", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	codeID := fs.String("code-id", "", "public code id from the inviter (required)")
	emailFlag := fs.String("email", "", "account email (for keychain entry; default: prompt)")
	deviceName := fs.String("device-name", "", "human-readable device name (default: hostname)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*upstream = pickUpstream(*upstream)
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "pair accept: --upstream required (or set $RESLEEVE_UPSTREAM)")
		return 2
	}
	if *codeID == "" {
		fmt.Fprintln(os.Stderr, "pair accept: --code-id required")
		return 2
	}
	if *deviceName == "" {
		*deviceName = hostnameOrDefault()
	}

	pairCode, err := promptPassword("pair code: ") // hide from shoulder-surfers
	if err != nil {
		return 1
	}
	// Canonicalize: ToUpper + TrimSpace + strip dashes, then re-insert
	// dashes in the inviter's 4-4-4 shape. The verifier hash is keyed
	// on the dashed form (formatPairCode emits it that way) so a user
	// pasting the code without dashes would otherwise mismatch.
	pairCode = canonicalizePairCode(pairCode)
	if pairCode == "" {
		fmt.Fprintln(os.Stderr, "pair accept: empty pair code")
		return 1
	}

	// We don't know the verifier salt until the server gives it to us
	// (PairClaimResp embeds wrapped params + salts). v1 simplification:
	// the published code envelope ships salts in the clear (they're
	// per-code random anyway), and the server's ConstantTimeCompare on
	// verifier_hash is what gates KEK release. So: pre-compute the
	// verifier hash using the params from a probe round-trip. We
	// inline the probe + claim into one round trip below by speculating
	// the params; if they don't match we re-derive.

	// To keep the v1 wire minimal we use the *same* default params the
	// inviter uses (16 MiB / 2 iters / 1). A future revision can ship
	// a /pair/probe route returning (params, verifier_salt) so the
	// inviter can rotate without coordination.
	params := auth.Argon2idParams{MemoryKiB: 16 * 1024, TimeIters: 2, Parallelism: 1}

	// The salt embedded in the publish envelope is private to the server
	// until the verifier check passes — but the v1 server does NOT echo
	// it back on rejection (no oracle). We POST the verifier_hash; if
	// the server's stored verifier matches, it returns the wrapped
	// envelope. So we precompute the hash using the same NewVerifier
	// recipe... but NewVerifier generates a fresh salt every time.
	//
	// Solution: include the salt-derivation in the published envelope's
	// public half. v1 ships salt = HKDF-Extract over (code_id, "pair-verifier")
	// so the client can derive it locally from code_id. Implemented
	// inline as a deterministic salt derivation.
	verifierSalt := auth.PairDeterministicSalt(*codeID, auth.PairVerifierLabel)
	verifierHash := auth.DeriveKey([]byte(pairCode), verifierSalt, params)

	var resp serve.PairClaimResp
	if err := postJSON(ctx, *upstream+"/v2/auth/pair/claim", "", serve.PairClaimReq{
		CodeID:       *codeID,
		VerifierHash: verifierHash,
		Device:       serve.DeviceMetadata{Name: *deviceName},
	}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "pair accept:", err)
		return 1
	}

	// Unwrap KEK using the same pair code. The KEK envelope ships its
	// own salt (returned by the server) so we don't reuse the verifier salt.
	wrapped := auth.WrappedKEK{
		Salt: resp.Wrapped.Salt, Nonce: resp.Wrapped.Nonce, Ciphertext: resp.Wrapped.CT,
		Params: auth.Argon2idParams{
			MemoryKiB: resp.Params.MemoryKiB, TimeIters: resp.Params.TimeIters, Parallelism: resp.Params.Parallelism,
		},
	}
	kek, err := wrapped.Unwrap([]byte(pairCode))
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair accept: unwrap KEK (pair code mismatch?):", err)
		return 1
	}

	// Persist device token under (upstream, email). Email is just a label
	// here — pairing doesn't require knowing the master password.
	email := strings.ToLower(strings.TrimSpace(*emailFlag))
	if email == "" {
		got, _ := promptLine("email (for local label): ")
		email = strings.ToLower(strings.TrimSpace(got))
	}
	kc, err := defaultKeychain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pair accept: keychain:", err)
		return 1
	}
	if email != "" {
		if err := kc.Put(*upstream, email, resp.DeviceToken); err != nil {
			fmt.Fprintln(os.Stderr, "pair accept: keychain put:", err)
			return 1
		}
	}

	// Install KEK in the local daemon. Same path as `resleeve login`.
	if err := unlockLocalDaemon(ctx, kek[:]); err != nil {
		fmt.Fprintln(os.Stderr, "pair accept: daemon unlock skipped:", err)
		fmt.Fprintln(os.Stderr, "       (start daemon with `resleeve up --upstream`, then re-run pair accept)")
	} else {
		fmt.Println("pair accept: daemon sealer installed ✓")
	}

	fmt.Println("pair accept ✓")
	fmt.Printf("  user id     %s\n", resp.UserID)
	fmt.Printf("  device id   %s\n", resp.DeviceID)
	fmt.Println()
	fmt.Println("Initial sync will start automatically.")
	return 0
}

// newPublicID returns a hex public id used for the pair code's CodeID.
// 8 random bytes = 16 hex chars. Matches the server's newID(8) shape.
func newPublicID(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, nBytes*2)
	for i, by := range b {
		out[2*i] = hexDigits[by>>4]
		out[2*i+1] = hexDigits[by&0x0f]
	}
	return string(out)
}

// formatPairCode chunks the random bytes into base32 segments
// separated by '-' for readability: e.g. "K7Q4-9XYZ-MTPB".
// 8 random bytes → 13 base32 chars (no padding) → 3 four-char groups
// + one trailing single char which we drop to keep the 12-char shape.
func formatPairCode(b []byte) string {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	if len(enc) > 12 {
		enc = enc[:12]
	}
	var parts []string
	for i := 0; i < len(enc); i += 4 {
		end := i + 4
		if end > len(enc) {
			end = len(enc)
		}
		parts = append(parts, enc[i:end])
	}
	return strings.Join(parts, "-")
}

// canonicalizePairCode normalizes a user-supplied pair code to the
// inviter-formatted shape ("XXXX-XXXX-XXXX"). Accepts the same code
// with or without dashes, with surrounding whitespace, in any case.
// Returns "" if the input is empty after stripping.
func canonicalizePairCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	if s == "" {
		return ""
	}
	var parts []string
	for i := 0; i < len(s); i += 4 {
		end := i + 4
		if end > len(s) {
			end = len(s)
		}
		parts = append(parts, s[i:end])
	}
	return strings.Join(parts, "-")
}
