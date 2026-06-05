//go:build windows

package cli

import (
	"os/exec"
	"testing"
	"time"
)

// TestProcessAliveAndTerminateDaemon_Windows proves the Windows primitives:
// processAlive reports a live child as running, terminateDaemon ends it,
// and processAlive then flips to false. We use `ping -n 30 127.0.0.1` as a
// dependency-free ~30s sleep (it ignores stdin, unlike `pause`/`timeout`).
func TestProcessAliveAndTerminateDaemon_Windows(t *testing.T) {
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid

	time.Sleep(150 * time.Millisecond)

	if !processAlive(pid) {
		t.Fatalf("processAlive(%d) = false, want true while child runs", pid)
	}

	if err := terminateDaemon(pid); err != nil {
		t.Fatalf("terminateDaemon(%d): %v", pid, err)
	}
	_ = cmd.Wait()

	// The exit-code flip after TerminateProcess isn't instantaneous; poll.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("processAlive(%d) = true after terminateDaemon, want false", pid)
}
