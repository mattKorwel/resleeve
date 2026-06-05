//go:build windows

package cli

import (
	"os"
	"syscall"
)

// stillActive is the Windows STILL_ACTIVE exit code (259) reported by
// GetExitCodeProcess while a process is running. It is not exported by
// the stdlib syscall package (nor golang.org/x/sys/windows), so we define
// it locally.
const stillActive = 259

// terminateDaemon stops the daemon. Windows has no stdlib-clean graceful
// SIGTERM equivalent for our use, so this is a hard kill: the daemon's
// context-cancel cleanup path won't run, which is tolerable for v1.
func terminateDaemon(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// daemonSysProcAttr detaches the spawned daemon from the parent's console
// by putting it in a new process group.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// processAlive reports whether a process with the given pid is still
// running, via OpenProcess + GetExitCodeProcess: a live process reports
// STILL_ACTIVE (259) as its exit code.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
