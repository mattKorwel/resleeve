//go:build unix

package cli

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

// TestProcessAliveAndTerminateDaemon_Unix proves the unix primitives:
// processAlive tracks a live child, and terminateDaemon delivers SIGTERM
// (not a hard kill) — the child traps TERM and exits with a marker code so
// we can tell the difference.
func TestProcessAliveAndTerminateDaemon_Unix(t *testing.T) {
	const marker = 42
	cmd := exec.Command("sh", "-c", `trap 'exit 42' TERM; while :; do sleep 0.05; done`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid

	// Let the child install its TERM trap before we signal it.
	time.Sleep(150 * time.Millisecond)

	if !processAlive(pid) {
		t.Fatalf("processAlive(%d) = false, want true while child runs", pid)
	}

	if err := terminateDaemon(pid); err != nil {
		t.Fatalf("terminateDaemon(%d): %v", pid, err)
	}

	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child Wait: got %v, want *exec.ExitError with code %d", err, marker)
	}
	if got := exitErr.ExitCode(); got != marker {
		t.Errorf("child exit code = %d, want %d (SIGTERM trap should have fired)", got, marker)
	}

	if processAlive(pid) {
		t.Errorf("processAlive(%d) = true after child exit, want false", pid)
	}
}
