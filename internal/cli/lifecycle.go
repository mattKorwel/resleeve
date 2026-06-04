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
func runDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	backfillCounts := fs.Bool("backfill-counts", false, "recompute sessions.event_count for every session (one-shot maintenance pass)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Maintenance passes short-circuit the cards. Each flag prints its
	// own outcome line and returns; they don't compose with the status
	// report (or with each other) in a single run.
	if *backfillCounts {
		return runDoctorBackfillCounts(ctx)
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
	if alive, pid := daemonAlive(); alive {
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
	settingsPath, _ := claudeSettingsPath()
	if settingsPath != "" {
		if data, err := os.ReadFile(settingsPath); err == nil {
			if strings.Contains(string(data), `"_resleeve": true`) {
				fmt.Printf("  bridge (claude)  ✓ installed in %s\n", settingsPath)
			} else {
				fmt.Printf("  bridge (claude)  ✗ not installed in %s\n", settingsPath)
			}
		} else {
			fmt.Printf("  bridge (claude)  ? settings.json missing\n")
		}
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
	return 0
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
