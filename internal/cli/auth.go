package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/serve"
	"golang.org/x/term"
)

// runRegister implements `resleeve register [--upstream URL] [--device-name N]`.
// Interactive: prompts for email + master password + password confirm.
// Derives KEK + verifier + wraps client-side (round-2/10 design — server
// never sees plaintext password or KEK). On success:
//   - persists the device token in the local keychain
//   - prints the recovery key ONCE (user must save)
//   - prints next-steps hint about `resleeve login` to actually unlock
//     the local daemon sealer (that lands in #3 commit 2)
func runRegister(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	deviceName := fs.String("device-name", "", "human-readable device name (default: hostname)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*upstream = pickUpstream(*upstream)
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "register: --upstream required (or set $RESLEEVE_UPSTREAM)")
		return 2
	}
	if *deviceName == "" {
		*deviceName = hostnameOrDefault()
	}

	email, err := promptLine("email: ")
	if err != nil {
		return 1
	}
	pw, err := promptPassword("master password: ")
	if err != nil {
		return 1
	}
	pw2, err := promptPassword("confirm password: ")
	if err != nil {
		return 1
	}
	if pw != pw2 {
		fmt.Fprintln(os.Stderr, "register: passwords do not match")
		return 1
	}

	// Mint the full crypto material locally. auth.Signup does all the
	// Argon2id, KEK generation, and dual-wrap — same code path the
	// round-2 unit tests cover. We never send the password or KEK over
	// the wire; only the verifier hashes + WrappedKEK envelopes go.
	signup, err := auth.Signup(email, pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		return 1
	}

	req := serve.RegisterReq{
		Email: signup.User.Email,
		Params: serve.Argon2idParams{
			MemoryKiB:   signup.User.Params.MemoryKiB,
			TimeIters:   signup.User.Params.TimeIters,
			Parallelism: signup.User.Params.Parallelism,
		},
		Password: serve.PasswordEnv{
			VerifierSalt: signup.User.PasswordVerifier.Salt,
			VerifierHash: signup.User.PasswordVerifier.Hash,
			KEKSalt:      signup.User.PasswordKEK.Salt,
			KEKNonce:     signup.User.PasswordKEK.Nonce,
			KEKCT:        signup.User.PasswordKEK.Ciphertext,
		},
		Recovery: serve.PasswordEnv{
			VerifierSalt: signup.User.RecoveryVerifier.Salt,
			VerifierHash: signup.User.RecoveryVerifier.Hash,
			KEKSalt:      signup.User.RecoveryKEK.Salt,
			KEKNonce:     signup.User.RecoveryKEK.Nonce,
			KEKCT:        signup.User.RecoveryKEK.Ciphertext,
		},
		Device: serve.DeviceMetadata{Name: *deviceName},
	}

	var resp serve.RegisterResp
	if err := postJSON(ctx, *upstream+"/v2/auth/register", "", req, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		return 1
	}

	kc, err := defaultKeychain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "register: keychain:", err)
		return 1
	}
	if err := kc.Put(*upstream, signup.User.Email, resp.DeviceToken); err != nil {
		fmt.Fprintln(os.Stderr, "register: keychain put:", err)
		return 1
	}

	fmt.Println()
	fmt.Println("registered ✓")
	fmt.Printf("  user id     %s\n", resp.UserID)
	fmt.Printf("  device id   %s\n", resp.DeviceID)
	fmt.Printf("  email       %s\n", signup.User.Email)
	fmt.Println()
	fmt.Println("RECOVERY KEY — save this somewhere safe. It is shown ONCE.")
	fmt.Println("If you lose your password AND this key, encrypted content is unrecoverable.")
	fmt.Println()
	fmt.Printf("  %s\n", signup.RecoveryKey)
	fmt.Println()
	fmt.Println("Next: `resleeve login` to unlock the local daemon sealer.")
	return 0
}

// runLogin implements `resleeve login`. Prompts master password,
// performs the Argon2id verifier challenge against the server,
// receives the WrappedKEK, unwraps locally, and (in commit 2)
// installs the resulting Sealer into the running daemon.
func runLogin(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	emailFlag := fs.String("email", "", "skip the interactive email prompt")
	deviceName := fs.String("device-name", "", "human-readable device name (default: hostname)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*upstream = pickUpstream(*upstream)
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "login: --upstream required (or set $RESLEEVE_UPSTREAM)")
		return 2
	}
	if *deviceName == "" {
		*deviceName = hostnameOrDefault()
	}
	email := *emailFlag
	if email == "" {
		got, err := promptLine("email: ")
		if err != nil {
			return 1
		}
		email = got
	}
	pw, err := promptPassword("master password: ")
	if err != nil {
		return 1
	}

	// 1) Fetch the verifier salt + Argon2 params from the server.
	var chal serve.LoginChallengeResp
	if err := postJSON(ctx, *upstream+"/v2/auth/login-challenge", "",
		serve.LoginChallengeReq{Email: strings.ToLower(strings.TrimSpace(email))}, &chal); err != nil {
		fmt.Fprintln(os.Stderr, "login: challenge:", err)
		return 1
	}
	params := auth.Argon2idParams{
		MemoryKiB:   chal.Params.MemoryKiB,
		TimeIters:   chal.Params.TimeIters,
		Parallelism: chal.Params.Parallelism,
	}

	// 2) Derive the verifier hash client-side and ship it.
	verifier := auth.DeriveKey([]byte(pw), chal.VerifierSalt, params)
	var resp serve.LoginResp
	if err := postJSON(ctx, *upstream+"/v2/auth/login", "",
		serve.LoginReq{Email: strings.ToLower(strings.TrimSpace(email)), VerifierHash: verifier,
			Device: serve.DeviceMetadata{Name: *deviceName}}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "login: ", err)
		return 1
	}

	// 3) Unwrap the KEK locally. The password never leaves this process.
	wrapped := auth.WrappedKEK{
		Salt: resp.WrappedKEK.Salt, Nonce: resp.WrappedKEK.Nonce, Ciphertext: resp.WrappedKEK.CT,
		Params: auth.Argon2idParams{
			MemoryKiB: resp.Params.MemoryKiB, TimeIters: resp.Params.TimeIters, Parallelism: resp.Params.Parallelism,
		},
	}
	kek, err := wrapped.Unwrap([]byte(pw))
	if err != nil {
		fmt.Fprintln(os.Stderr, "login: unwrap KEK (likely wrong password):", err)
		return 1
	}

	// 4) Persist the device token in the keychain. Before writing, run
	//    the one-shot file → OS keychain migration for this
	//    (upstream, email) pair so users upgrading from the pre-follow-
	//    up-A file backend don't lose existing device tokens.
	kc, err := defaultKeychain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "login: keychain:", err)
		return 1
	}
	normEmail := strings.ToLower(strings.TrimSpace(email))
	maybeMigrateFromFile(kc, *upstream, normEmail)
	if err := kc.Put(*upstream, normEmail, resp.DeviceToken); err != nil {
		fmt.Fprintln(os.Stderr, "login: keychain put:", err)
		return 1
	}

	// 5) Push the unwrapped KEK to the local daemon's /v1/seal/unlock so
	//    background sync can decrypt pulled blobs and encrypt outbound
	//    ones. Best-effort: if the daemon isn't up, we still consider
	//    login a success — the daemon will pick up the token at startup.
	if err := unlockLocalDaemon(ctx, kek[:]); err != nil {
		fmt.Fprintln(os.Stderr, "login: daemon unlock skipped:", err)
		fmt.Fprintln(os.Stderr, "       (start the daemon with `resleeve up`, then `resleeve login` again)")
	} else {
		fmt.Println("login: daemon sealer installed ✓")
	}

	fmt.Println("login ✓")
	fmt.Printf("  user id    %s\n", resp.UserID)
	fmt.Printf("  device id  %s\n", resp.DeviceID)
	return 0
}

// runLogout revokes the device token server-side and removes it from
// the local keychain. Also locks the local daemon's sealer.
func runLogout(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	emailFlag := fs.String("email", "", "account to log out (default: prompt)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*upstream = pickUpstream(*upstream)
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "logout: --upstream required (or set $RESLEEVE_UPSTREAM)")
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

	kc, err := defaultKeychain()
	if err != nil {
		fmt.Fprintln(os.Stderr, "logout: keychain:", err)
		return 1
	}
	tok, err := kc.Get(*upstream, email)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logout: keychain get:", err)
		return 1
	}
	if tok == "" {
		fmt.Fprintln(os.Stderr, "logout: no device token on file for that (upstream, email)")
		return 1
	}
	// Best-effort server-side revoke. A network failure shouldn't keep
	// us from removing the local copy.
	if err := postJSON(ctx, *upstream+"/v2/auth/logout", tok, struct{}{}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "logout: server revoke failed (continuing):", err)
	}
	if err := kc.Delete(*upstream, email); err != nil {
		fmt.Fprintln(os.Stderr, "logout: keychain delete:", err)
		return 1
	}
	if err := lockLocalDaemon(ctx); err != nil {
		// Daemon down or no-such-route is fine — pre-#3 daemons don't
		// know /v1/seal/lock. Don't fail the whole logout for that.
		fmt.Fprintln(os.Stderr, "logout: daemon lock skipped:", err)
	}
	fmt.Println("logout ✓")
	return 0
}

// --- daemon helpers (used by login/logout/pair). The actual /v1/seal/*
// routes land in commit 2 of #3; these helpers POST to them. ---

func unlockLocalDaemon(ctx context.Context, kek []byte) error {
	c, err := clientFromEndpoint()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string][]byte{"kek": kek})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/seal/unlock", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon /v1/seal/unlock: %s", resp.Status)
	}
	return nil
}

func lockLocalDaemon(ctx context.Context) error {
	c, err := clientFromEndpoint()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/seal/lock", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("daemon /v1/seal/lock: %s", resp.Status)
	}
	return nil
}

// --- small helpers ---

func postJSON(ctx context.Context, endpoint, bearer string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		// Best-effort error JSON decode.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(errBody, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(errBody)))
	}
	if out == nil {
		// Drain so the keep-alive is preserved.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func pickUpstream(flagVal string) string {
	if flagVal != "" {
		return strings.TrimRight(flagVal, "/")
	}
	if env := os.Getenv("RESLEEVE_UPSTREAM"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return ""
}

func hostnameOrDefault() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "device"
}

func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", errors.New("no input")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	// If stdin isn't a TTY (piped input, tests), fall back to plain line read.
	fd := int(syscall.Stdin)
	if !term.IsTerminal(fd) {
		s, err := promptLine("")
		fmt.Fprintln(os.Stderr) // newline since user didn't see one
		return s, err
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// urlWithQuery appends a query string to base, handling existing '?'.
// Shared helper used by pair.go (now its own file).
func urlWithQuery(base string, q url.Values) string {
	if len(q) == 0 {
		return base
	}
	if strings.Contains(base, "?") {
		return base + "&" + q.Encode()
	}
	return base + "?" + q.Encode()
}

// keep time import used (will be in commit 3)
var _ = time.Now
