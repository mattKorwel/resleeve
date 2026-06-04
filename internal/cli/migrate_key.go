package cli

import (
	"bufio"
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

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/serve"
)

// runMigrateKey implements `resleeve migrate-key`. It re-encrypts
// upstream-stored rows from the round-4 placeholder seal.key under the
// new round-5 master-password-derived KEK, in place on the server.
//
// Flow:
//  1. Read the old 32-byte AES key from --seal-key=PATH (defaults to
//     ~/.resleeve/seal.key). Build the OLD AESGCMSealer.
//  2. Run the normal login challenge/login round-trip against --upstream
//     to recover the NEW KEK. Build the NEW AESGCMSealer.
//  3. For each kind (sessions, events, memory): paginate
//     GET /v2/sync/pull, for each row Open with OLD and Seal with NEW,
//     then POST /v2/sync/push with the same key (the server overwrites
//     in place — keys are content-addressed, not blob-addressed).
//  4. Best-effort POST /v1/seal/unlock to the local daemon so subsequent
//     captures encrypt under the new KEK.
//  5. Print next-steps for the operator to delete ~/.resleeve/seal.key
//     (we don't delete it implicitly — symmetric with `purge` UX).
//
// Idempotent: a partially-completed run can be rerun. Rows already
// re-encrypted under the new KEK fail to Open with old and are skipped
// (logged). Rows still under the old key continue migrating.
//
// Scope: only the per-user kinds that the round-4/02 sync layer wraps
// (sessions, events, memory). Server-side blobs that pre-date encryption
// (slice 2 pre-2.5) would already be plaintext; we don't try to detect
// that here — operators of legacy deployments are expected to be on a
// post-2.5 daemon.
func runMigrateKey(ctx context.Context, args []string) int {
	// Refuse to run while the daemon is up: a live daemon under the OLD
	// sealer could push mid-migration, producing mixed-key blobs on
	// upstream. Operator must `resleeve down` first. (round-5 follow-up D)
	if alive, pid := daemonAlive(); alive {
		fmt.Fprintf(os.Stderr, "migrate-key: daemon is running (pid %d) — run `resleeve down` first\n", pid)
		return 1
	}
	fs := flag.NewFlagSet("migrate-key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	emailFlag := fs.String("email", "", "skip the interactive email prompt")
	sealKey := fs.String("seal-key", defaultSealKeyPath(), "path to the legacy placeholder seal key (32 raw bytes)")
	dryRun := fs.Bool("dry-run", false, "report what would be migrated; don't push anything back")
	yes := fs.Bool("yes", false, "skip the pre-migration confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*upstream = pickUpstream(*upstream)
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "migrate-key: --upstream required (or set $RESLEEVE_UPSTREAM)")
		return 2
	}

	// 1) Old key.
	oldBytes, err := os.ReadFile(*sealKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate-key: read --seal-key %s: %v\n", *sealKey, err)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "       (no placeholder seal.key found — nothing to migrate)")
		}
		return 1
	}
	if len(oldBytes) != 32 {
		fmt.Fprintf(os.Stderr, "migrate-key: --seal-key %s: length %d, want 32\n", *sealKey, len(oldBytes))
		return 1
	}
	oldSealer, err := auth.NewAESGCMSealer(oldBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-key: build old sealer:", err)
		return 1
	}

	// 2) New KEK via login. We don't reuse runLogin because we want the
	// raw KEK bytes back; runLogin only stashes them on the daemon.
	email := *emailFlag
	if email == "" {
		got, err := promptLine("email: ")
		if err != nil {
			return 1
		}
		email = got
	}
	email = strings.ToLower(strings.TrimSpace(email))
	pw, err := promptPassword("master password: ")
	if err != nil {
		return 1
	}

	newKEK, deviceToken, err := loginAndUnwrapKEK(ctx, *upstream, email, pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-key:", err)
		return 1
	}
	newSealer, err := auth.NewAESGCMSealer(newKEK[:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-key: build new sealer:", err)
		return 1
	}

	if !*yes {
		fmt.Printf("About to re-encrypt all upstream rows for %s under the new master-password KEK.\n", email)
		fmt.Println("This is idempotent and safe to rerun, but cannot be undone without the old key file.")
		fmt.Printf("Keep %s until you've verified the new state.\n", *sealKey)
		fmt.Print("Type 'migrate' to confirm: ")
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() || strings.TrimSpace(sc.Text()) != "migrate" {
			fmt.Println("aborted.")
			return 1
		}
	}

	// 3) For each kind: pull → rewrap → push.
	totals := migrateTotals{}
	for _, kind := range []string{"sessions", "events", "memory"} {
		n, skipped, err := migrateKind(ctx, *upstream, deviceToken, kind, oldSealer, newSealer, *dryRun)
		totals.Migrated += n
		totals.Skipped += skipped
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate-key: %s: %v\n", kind, err)
			return 1
		}
		fmt.Printf("  %-8s  migrated=%d  skipped=%d\n", kind, n, skipped)
	}
	if *dryRun {
		fmt.Printf("dry-run: would have re-encrypted %d row(s); %d already on new KEK or unreadable.\n",
			totals.Migrated, totals.Skipped)
		return 0
	}
	fmt.Printf("migrate-key: re-encrypted %d row(s); %d already on new KEK or unreadable.\n",
		totals.Migrated, totals.Skipped)

	// 4) Best-effort daemon unlock so subsequent capture matches.
	if err := unlockLocalDaemon(ctx, newKEK[:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-key: daemon unlock skipped:", err)
	} else {
		fmt.Println("migrate-key: daemon sealer rotated ✓")
	}

	// 5) Next-steps. The placeholder seal.key is now retired by the
	// interactive helper, which also gates on the daemon being down.
	// We don't do it inline here because this verb may run while the
	// daemon is still sealing under the old key (it just got its
	// sealer rotated above).
	fmt.Println()
	fmt.Println("Next: verify `resleeve session list` / `resleeve resume` works, then")
	fmt.Println("retire the legacy placeholder:")
	fmt.Println("  resleeve down && resleeve doctor --migrate-key")
	return 0
}

type migrateTotals struct {
	Migrated int
	Skipped  int
}

// migrateKind paginates GET /v2/sync/pull for one kind, Open-with-old,
// Seal-with-new, then POST /v2/sync/push. Server-side keys are
// preserved; the backend Put is overwrite-in-place.
//
// A row whose Open fails under oldSealer is assumed to already be on
// the new KEK (or otherwise unreadable) and is skipped — this is what
// makes a partially-completed run safe to rerun.
func migrateKind(
	ctx context.Context,
	upstream, token, kind string,
	oldSealer, newSealer auth.Sealer,
	dryRun bool,
) (migrated int, skipped int, err error) {
	httpc := &http.Client{}
	cursor := ""
	for {
		u := fmt.Sprintf("%s/v2/sync/pull?kind=%s&since=%s&limit=200",
			upstream, kind, url.QueryEscape(cursor))
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if rerr != nil {
			return migrated, skipped, rerr
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, derr := httpc.Do(req)
		if derr != nil {
			return migrated, skipped, fmt.Errorf("pull: %w", derr)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return migrated, skipped, fmt.Errorf("pull %s: status %d: %s", kind, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var pull serve.PullResp
		if jerr := json.NewDecoder(resp.Body).Decode(&pull); jerr != nil {
			resp.Body.Close()
			return migrated, skipped, fmt.Errorf("decode pull: %w", jerr)
		}
		resp.Body.Close()
		if len(pull.Rows) == 0 {
			return migrated, skipped, nil
		}

		batch := make([]serve.PushRow, 0, len(pull.Rows))
		for _, row := range pull.Rows {
			plain, oerr := oldSealer.Open(row.Blob)
			if oerr != nil {
				// Already on new KEK, or otherwise can't be decrypted by
				// the old key. Either way, leave it alone.
				skipped++
				cursor = row.Key
				continue
			}
			sealed, serr := newSealer.Seal(plain)
			if serr != nil {
				return migrated, skipped, fmt.Errorf("seal new %s: %w", row.Key, serr)
			}
			batch = append(batch, serve.PushRow{Key: row.Key, Blob: sealed})
			cursor = row.Key
		}

		if len(batch) > 0 && !dryRun {
			if perr := pushBatch(ctx, httpc, upstream, token, batch); perr != nil {
				return migrated, skipped, perr
			}
		}
		migrated += len(batch)

		if pull.NextCursor == "" {
			return migrated, skipped, nil
		}
	}
}

func pushBatch(ctx context.Context, httpc *http.Client, upstream, token string, batch []serve.PushRow) error {
	body, err := json.Marshal(serve.PushReq{Batch: batch})
	if err != nil {
		return fmt.Errorf("marshal push: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream+"/v2/sync/push", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// loginAndUnwrapKEK runs the same challenge → login → unwrap sequence
// as `resleeve login`, but returns the raw KEK bytes (plus the fresh
// device token) so callers can use the KEK directly for re-encryption
// instead of just shoving it at the daemon. Splitting this out avoids
// reaching into runLogin's interactive prompt + stdout flow.
func loginAndUnwrapKEK(ctx context.Context, upstream, email, pw string) (auth.KEK, string, error) {
	var chal serve.LoginChallengeResp
	if err := postJSON(ctx, upstream+"/v2/auth/login-challenge", "",
		serve.LoginChallengeReq{Email: email}, &chal); err != nil {
		return auth.KEK{}, "", fmt.Errorf("login challenge: %w", err)
	}
	params := auth.Argon2idParams{
		MemoryKiB:   chal.Params.MemoryKiB,
		TimeIters:   chal.Params.TimeIters,
		Parallelism: chal.Params.Parallelism,
	}
	verifier := auth.DeriveKey([]byte(pw), chal.VerifierSalt, params)
	var resp serve.LoginResp
	if err := postJSON(ctx, upstream+"/v2/auth/login", "",
		serve.LoginReq{Email: email, VerifierHash: verifier,
			Device: serve.DeviceMetadata{Name: hostnameOrDefault()}}, &resp); err != nil {
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
	kek, err := wrapped.Unwrap([]byte(pw))
	if err != nil {
		return auth.KEK{}, "", fmt.Errorf("unwrap KEK (likely wrong password): %w", err)
	}
	return kek, resp.DeviceToken, nil
}

// runDoctorMigrateKey implements `resleeve doctor --migrate-key`. It is
// a tiny interactive helper: detect the placeholder seal.key, explain
// the migration model, and offer to delete the file (only after the
// user explicitly confirms). The actual upstream re-encryption is a
// separate verb (runMigrateKey) — this is just the local cleanup.
func runDoctorMigrateKey(_ context.Context) int {
	// Refuse while the daemon is up: it has the old key cached in-memory
	// and may still be sealing captures under it; deleting seal.key out
	// from under a live daemon is a footgun. (round-5 follow-up D)
	if alive, pid := daemonAlive(); alive {
		fmt.Fprintf(os.Stderr, "doctor --migrate-key: daemon is running (pid %d) — run `resleeve down` first\n", pid)
		return 1
	}
	path := defaultSealKeyPath()
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("no legacy seal.key at %s — you're already on the master-password KEK model ✓\n", path)
			return 0
		}
		fmt.Fprintln(os.Stderr, "doctor --migrate-key:", err)
		return 1
	}
	fmt.Println("Found legacy placeholder at:")
	fmt.Printf("  %s  (size=%d, mode=%o)\n", path, st.Size(), st.Mode().Perm())
	fmt.Println()
	fmt.Println("Round-5 retires this file in favor of a KEK derived from your master password.")
	fmt.Println("If you've pushed encrypted data to an upstream serve, run:")
	fmt.Println("    resleeve migrate-key --upstream <url>")
	fmt.Println("FIRST, so the server-side blobs are re-wrapped under the new KEK.")
	fmt.Println()
	fmt.Print("Delete this file now? Type 'yes' to confirm: ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "yes" {
		fmt.Println("kept in place.")
		return 0
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintln(os.Stderr, "doctor --migrate-key: remove:", err)
		return 1
	}
	fmt.Println("removed ✓")
	return 0
}
