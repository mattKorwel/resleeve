//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
)

// execResume runs the native resume command as a child process, wiring the
// parent's stdio through, then exits with the child's exit code. Windows
// has no exec-replace; this mimics it as closely as possible so the verb's
// exit status mirrors the child. It returns only when the child could not
// be started.
func execResume(cmdPath string, argv []string, env []string) error {
	cmd := exec.Command(cmdPath, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		// Failed to start / spawn the child — let the caller report it.
		return err
	}
	os.Exit(0)
	return nil // unreachable
}
