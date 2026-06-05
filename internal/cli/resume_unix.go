//go:build unix

package cli

import "syscall"

// execResume replaces this process image with the native resume command.
// On success it never returns — the target CLI inherits our pid, tty,
// signals, and env directly.
func execResume(cmdPath string, argv []string, env []string) error {
	return syscall.Exec(cmdPath, argv, env)
}
