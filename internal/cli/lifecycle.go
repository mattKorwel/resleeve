package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/agent"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
)

// runUp installs bridges + starts the daemon in the background.
func runUp(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	noBridge := fs.Bool("no-bridge", false, "skip bridge plugin install")
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	upstreamToken := fs.String("upstream-token", "", "v2 sync bearer token (default: $RESLEEVE_UPSTREAM_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *upstream == "" {
		*upstream = os.Getenv("RESLEEVE_UPSTREAM")
	}
	if *upstreamToken == "" {
		*upstreamToken = os.Getenv("RESLEEVE_UPSTREAM_TOKEN")
	}

	dataDir, err := agent.DataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "up:", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "up: mkdir data dir:", err)
		return 1
	}

	// Check whether a daemon is already running.
	if alive, pid := daemonAlive(); alive {
		fmt.Printf("[1/2] daemon already running (pid %d) — skipping\n", pid)
	} else {
		if err := spawnDaemon(dataDir, *upstream, *upstreamToken); err != nil {
			fmt.Fprintln(os.Stderr, "up: spawn daemon:", err)
			return 1
		}
		// Wait for endpoint + health.
		if err := waitForDaemon(5 * time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "up: daemon didn't come up:", err)
			return 1
		}
		url, _, _ := agent.LoadEndpoint()
		fmt.Printf("[1/2] daemon started at %s\n", url)
	}

	if !*noBridge {
		a, err := pickAdapter("claude")
		if err != nil {
			fmt.Fprintln(os.Stderr, "up:", err)
			return 1
		}
		if err := a.InstallBridge(ctx, adapter.InstallOpts{}); err != nil {
			fmt.Fprintln(os.Stderr, "up: install bridge:", err)
			return 1
		}
		fmt.Println("[2/2] installed bridge (claude)")
	} else {
		fmt.Println("[2/2] skipped bridge install (--no-bridge)")
	}
	fmt.Println("resleeve is up.")
	return 0
}

// runDown stops the daemon and removes bridges.
func runDown(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keepBridge := fs.Bool("keep-bridge", false, "leave bridge hooks installed")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if alive, pid := daemonAlive(); alive {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			fmt.Fprintln(os.Stderr, "down: kill daemon:", err)
			return 1
		}
		// Wait briefly for cleanup.
		for i := 0; i < 30; i++ {
			if a, _ := daemonAlive(); !a {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("[1/2] daemon stopped (pid %d)\n", pid)
	} else {
		fmt.Println("[1/2] daemon not running")
	}

	if !*keepBridge {
		a, err := pickAdapter("claude")
		if err != nil {
			fmt.Fprintln(os.Stderr, "down:", err)
			return 1
		}
		if err := a.UninstallBridge(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "down: uninstall bridge:", err)
			return 1
		}
		fmt.Println("[2/2] removed bridge (claude)")
	} else {
		fmt.Println("[2/2] kept bridge install (--keep-bridge)")
	}
	return 0
}

// runPurge wipes the data dir after explicit confirmation.
func runPurge(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dataDir, err := agent.DataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "purge:", err)
		return 1
	}

	if alive, pid := daemonAlive(); alive {
		fmt.Fprintln(os.Stderr, "purge: daemon is running (pid", pid, ") — run `resleeve down` first")
		return 1
	}

	if !*yes {
		fmt.Printf("This will delete %s permanently.\nType 'purge' to confirm: ", dataDir)
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() || strings.TrimSpace(sc.Text()) != "purge" {
			fmt.Println("aborted.")
			return 1
		}
	}

	if err := os.RemoveAll(dataDir); err != nil {
		fmt.Fprintln(os.Stderr, "purge:", err)
		return 1
	}
	fmt.Println("purged.")
	return 0
}

// runDoctor reports daemon + bridge + CLI status.
//
// The exit code is non-zero when doctor detects the F13 silent-no-op
// state: bridge installed (✓) but daemon not running (✗). In that
// configuration hooks fire but emit nothing — looks half-installed
// from the user's perspective. Loud exit code so scripted callers
// (CI, smoke tests, "resleeve doctor && resleeve up") notice.
func runDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	backfillCounts := fs.Bool("backfill-counts", false, "recompute sessions.event_count for every session (one-shot maintenance pass)")
	backfillCwd := fs.Bool("backfill-cwd", false, "repair sessions.cwd for pre-existing reconcile-only sessions (#6)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Maintenance passes short-circuit the cards. Each flag prints its
	// own outcome line and returns; they don't compose with the status
	// report (or with each other) in a single run. --backfill-cwd in
	// particular requires the daemon to be DOWN (raw UPDATEs bypass
	// the ingest pipeline).
	if *backfillCounts {
		return runDoctorBackfillCounts(ctx)
	}
	if *backfillCwd {
		return runDoctorBackfillCwd(ctx)
	}

	fmt.Println("resleeve doctor")
	fmt.Println("===============")

	// Data dir
	dataDir, err := agent.DataDir()
	if err == nil {
		st, _ := os.Stat(dataDir)
		if st == nil {
			fmt.Printf("  data dir         %s (missing)\n", dataDir)
		} else {
			fmt.Printf("  data dir         %s\n", dataDir)
		}
	}

	// Daemon
	daemonRunning, _ := daemonAlive()
	if daemonRunning {
		_, pid := daemonAlive()
		fmt.Printf("  daemon           ✓ running (pid %d)\n", pid)
		if url, _, _ := agent.LoadEndpoint(); url != "" {
			fmt.Printf("  endpoint         %s\n", url)
			// Try /v1/health.
			resp, err := http.Get(url + "/v1/health")
			if err == nil && resp.StatusCode == 200 {
				resp.Body.Close()
				fmt.Println("  /v1/health       ✓ 200 OK")
			} else {
				fmt.Printf("  /v1/health       ✗ %v\n", err)
			}
		}
	} else {
		fmt.Println("  daemon           ✗ not running")
	}

	// Bridge
	bridgeInstalled := false
	settingsPath, _ := claudeSettingsPath()
	if settingsPath != "" {
		if data, err := os.ReadFile(settingsPath); err == nil {
			if strings.Contains(string(data), `"_resleeve": true`) {
				bridgeInstalled = true
				fmt.Printf("  bridge (claude)  ✓ installed in %s\n", settingsPath)
			} else {
				fmt.Printf("  bridge (claude)  ✗ not installed in %s\n", settingsPath)
			}
		} else {
			fmt.Printf("  bridge (claude)  ? settings.json missing\n")
		}
	}

	// Sync cards (upstream / slow / fast) — additive helpers; only
	// useful when the daemon is up since the snapshot lives in-process.
	if daemonRunning {
		printSyncCards(ctx)
	}

	// CLI binary
	if claudeBin, err := exec.LookPath("claude"); err == nil {
		fmt.Printf("  claude binary    %s\n", claudeBin)
	} else {
		fmt.Println("  claude binary    ✗ not on $PATH")
	}

	// Resleeve binary self
	if self, err := os.Executable(); err == nil {
		fmt.Printf("  resleeve binary  %s\n", self)
	}

	// Hook-env card: LOUDLY warn when bridge is installed but daemon
	// is not running — the F13 silent-injection-failure state. Returns
	// non-zero exit code so scripted callers notice.
	return printHookEnvCard(bridgeInstalled, daemonRunning)
}

// printSyncCards renders the upstream / sync(slow) / sync(fast-sse)
// cards. Pulls the snapshot from the daemon's /v1/doctor/sync-status
// endpoint. If the daemon is not reachable or the endpoint is
// unavailable (older daemon), prints nothing — the daemon ✗ line
// above already covered that case.
func printSyncCards(ctx context.Context) {
	c, err := clientFromEndpoint()
	if err != nil {
		return
	}
	snap, err := c.DoctorSyncStatus(ctx)
	if err != nil {
		fmt.Printf("  sync             ✗ couldn't fetch status: %v\n", err)
		return
	}
	printUpstreamCard(snap)
	printSyncSlowCard(snap)
	printSyncFastCard(snap)
}

// printUpstreamCard renders the upstream reachability line. When no
// upstream is configured we emit a single "standalone" line so users
// know sync just isn't a thing for this daemon.
func printUpstreamCard(snap *agent.SyncStatusSnapshot) {
	if snap.UpstreamConfig == "" {
		fmt.Println("  upstream         (standalone — no upstream configured)")
		return
	}
	if snap.UpstreamOK {
		fmt.Printf("  upstream         %s (✓ reachable, %dms RTT)\n", snap.UpstreamConfig, snap.UpstreamRTTms)
	} else {
		errStr := snap.UpstreamError
		if errStr == "" {
			errStr = "unreachable"
		}
		fmt.Printf("  upstream         %s (✗ %s)\n", snap.UpstreamConfig, errStr)
	}
}

// printSyncSlowCard renders the slow-tier card: last drain, last pull,
// outbox depth. Outbox depth > 0 with a stale drain timestamp is a
// hint that pushes are failing — though we don't try to diagnose
// why here.
func printSyncSlowCard(snap *agent.SyncStatusSnapshot) {
	drain := agoOrNever(snap.DrainLast)
	// "events" is the highest-volume kind and is the most useful single
	// signal for "is pull doing anything"; show its last-pull time as
	// the headline. (sessions and memory get rolled up into the same
	// loop so they all advance together in practice.)
	var lastPull time.Time
	for _, t := range snap.PullLastPerKind {
		if t.After(lastPull) {
			lastPull = t
		}
	}
	fmt.Printf("  sync (slow)      last drain %s • last pull %s • outbox depth %d\n",
		drain, agoOrNever(lastPull), snap.OutboxDepth)
}

// printSyncFastCard renders the fast-tier (SSE) card. Three states:
// connected (with uptime + last event), disconnected (showing last
// known event), and never-connected (likely no upstream).
func printSyncFastCard(snap *agent.SyncStatusSnapshot) {
	if snap.UpstreamConfig == "" {
		// No upstream → fast tier doesn't apply. Skip the card to
		// keep the output uncluttered.
		return
	}
	if snap.SSEConnected {
		uptime := time.Duration(snap.SSEUptimeSec) * time.Second
		fmt.Printf("  sync (fast/sse)  ✓ connected (%s uptime) • last event %s\n",
			compactDuration(uptime), agoOrNever(snap.SSELastEventAt))
		return
	}
	if snap.SSEConnectedAt.IsZero() {
		fmt.Println("  sync (fast/sse)  ✗ never connected")
		return
	}
	fmt.Printf("  sync (fast/sse)  ✗ disconnected • last event %s\n", agoOrNever(snap.SSELastEventAt))
}

// printHookEnvCard renders the hook-env card and returns the exit
// code for runDoctor. The F13 dogfood finding: a daemon-down +
// bridge-installed config is the silent-no-op state — the hook
// fires, gets connection-refused, and CC sees no additionalContext.
// Looks half-installed. We yell with red+bold ANSI escapes (only on
// a TTY) and exit non-zero so scripts catch it.
func printHookEnvCard(bridgeInstalled, daemonRunning bool) int {
	switch {
	case bridgeInstalled && !daemonRunning:
		// The dangerous combo. Loud.
		msg := "bridge ✓ — but daemon ✗ ! HOOKS ARE NO-OPS — run `resleeve up`"
		if isStdoutTTY() {
			// Bright red, bold.
			fmt.Printf("  hook env         \x1b[1;31m%s\x1b[0m\n", msg)
		} else {
			fmt.Printf("  hook env         %s\n", msg)
		}
		return 1
	case bridgeInstalled && daemonRunning:
		fmt.Println("  hook env         ✓ bridge + daemon both up")
		return 0
	case !bridgeInstalled && daemonRunning:
		fmt.Println("  hook env         bridge ✗ — daemon up but hooks not wired (run `resleeve up`)")
		return 0
	default:
		// Neither bridge nor daemon. This is the post-`resleeve down`
		// state — annoying to flag loudly, so just note it.
		fmt.Println("  hook env         ✗ neither bridge nor daemon — run `resleeve up`")
		return 0
	}
}

// agoOrNever formats a timestamp as "Ns ago" / "Nm ago" / "Nh ago" or
// "never" if the time is the zero value. Sub-second resolution is
// rolled up to "just now".
func agoOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	return compactDuration(d) + " ago"
}

// compactDuration renders a duration as "Ns" / "Nm" / "Nh" rounded down.
// Sub-second collapses to "0s" (then surfaced as "just now" by callers).
func compactDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// isStdoutTTY reports whether stdout looks like an interactive
// terminal. Used to decide whether to emit ANSI color escapes in
// the loud hook-env warning. Conservative: anything we can't stat
// or any non-character-device is treated as non-TTY (so piped /
// redirected output stays clean).
func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// runDoctorBackfillCounts recomputes sessions.event_count for every row
// in the local DB. Cleanup pass for sessions captured before the F7 fix
// (commit fde1c40) wired SyncEventCount into the IngestBatch path —
// pre-F7 rows stayed at event_count=0 until their next batch arrived.
// Safe to re-run; SyncEventCount is COUNT(*) over events, not a delta.
func runDoctorBackfillCounts(ctx context.Context) int {
	store, err := sqlite.Open(ctx, defaultDSN())
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor: open store:", err)
		return 1
	}
	defer store.Close()

	sessions, err := store.Sessions().List(ctx, rsql.SessionFilter{Limit: -1})
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor: list sessions:", err)
		return 1
	}
	var recomputed, failed int
	for i, s := range sessions {
		if err := store.Sessions().SyncEventCount(ctx, s.ID); err != nil {
			fmt.Fprintf(os.Stderr, "doctor: sync %s: %v\n", s.ID, err)
			failed++
			continue
		}
		recomputed++
		if (i+1)%100 == 0 {
			fmt.Printf("  scanned %d/%d sessions...\n", i+1, len(sessions))
		}
	}
	fmt.Printf("recomputed %d session event_counts", recomputed)
	if failed > 0 {
		fmt.Printf(" (%d failed)", failed)
	}
	fmt.Println(".")
	if failed > 0 {
		return 1
	}
	return 0
}

// runDoctorBackfillCwd repairs sessions.cwd for pre-existing
// reconcile-only sessions (polish punch list #6). It refuses to run
// when the daemon is up: the migration performs raw UPDATEs that
// bypass the ingest pipeline, and a live daemon could re-overwrite
// rows from a concurrent reconcile sweep.
func runDoctorBackfillCwd(ctx context.Context) int {
	if alive, pid := daemonAlive(); alive {
		fmt.Fprintf(os.Stderr, "doctor --backfill-cwd: daemon is running (pid %d) — run `resleeve down` first\n", pid)
		return 1
	}
	st, err := sqlite.Open(ctx, defaultDSN())
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor --backfill-cwd: open store:", err)
		return 1
	}
	defer st.Close()

	logf := func(format string, args ...any) { fmt.Printf(format, args...) }
	repaired, skipped, err := agent.BackfillSessionCwd(ctx, st, logf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor --backfill-cwd:", err)
		return 1
	}
	fmt.Printf("scanned: repaired %d, skipped %d\n", repaired, skipped)
	return 0
}

// runUsage prints storage stats.
func runUsage(ctx context.Context, args []string) int {
	dataDir, err := agent.DataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		return 1
	}
	totalBytes, err := dirSize(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		return 1
	}
	fmt.Printf("data dir: %s\n", dataDir)
	fmt.Printf("total:    %s\n", humanBytes(totalBytes))

	c, cerr := clientFromEndpoint()
	if cerr != nil {
		fmt.Println("(daemon not running — can't fetch session/event counts)")
		return 0
	}
	sessions, err := c.ListSessions(ctx, rsql.SessionFilter{Limit: 500})
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: list sessions:", err)
		return 1
	}
	fmt.Printf("sessions: %d\n", len(sessions))

	// Compute total events by summing over a wider page.
	var totalEvents int64
	for _, s := range sessions {
		totalEvents += s.EventCount
	}
	if totalEvents == 0 {
		// event_count isn't auto-incremented in v1; query via search hack
		// is too coarse. Skip when unknown.
		fmt.Println("events:   (per-session counts not yet tracked in v1)")
	} else {
		fmt.Printf("events:   %d\n", totalEvents)
	}
	return 0
}

// --- helpers ---

func spawnDaemon(dataDir, upstream, upstreamToken string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve resleeve binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err == nil {
		self = resolved
	}

	logPath, _ := agent.LogPath()
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", logPath, err)
	}

	dsn := defaultDSN()
	cmdArgs := []string{"agent", "--dsn", dsn, "--addr", "127.0.0.1:0"}
	if upstream != "" {
		cmdArgs = append(cmdArgs, "--upstream", upstream)
	}
	if upstreamToken != "" {
		cmdArgs = append(cmdArgs, "--upstream-token", upstreamToken)
	}
	cmd := exec.Command(self, cmdArgs...)
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Daemon is now its own session leader. Parent exits; child lives on.
	_ = cmd.Process.Release()
	return nil
}

func waitForDaemon(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if url, _, err := agent.LoadEndpoint(); err == nil && url != "" {
			resp, herr := http.Get(url + "/v1/health")
			if herr == nil && resp.StatusCode == 200 {
				resp.Body.Close()
				return nil
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timed out waiting for /v1/health")
}

func daemonAlive() (bool, int) {
	pidPath, err := agent.PIDPath()
	if err != nil {
		return false, 0
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false, 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false, 0
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, pid
	}
	// Signal 0: just probe whether the process exists / we can signal it.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false, pid
	}
	return true, pid
}

func claudeSettingsPath() (string, error) {
	// Use $HOME-respecting lookup so doctor agrees with InstallBridge
	// when running under a test HOME override.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
