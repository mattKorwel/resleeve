//go:build unix

package cli

import (
	"os"
	"syscall"
)

// terminateDaemon asks the daemon to shut down gracefully by delivering
// SIGTERM, letting its context-cancel cleanup path run.
func terminateDaemon(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// daemonSysProcAttr detaches the spawned daemon into its own session so it
// outlives the parent's controlling terminal.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether a process with the given pid currently
// exists. Signal 0 probes existence without actually delivering a signal.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
