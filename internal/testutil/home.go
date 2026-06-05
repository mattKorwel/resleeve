// Package testutil holds small cross-platform helpers shared across the
// repo's test suites.
package testutil

import "testing"

// SetHomeDir points os.UserHomeDir() at dir for the duration of the test
// on every platform. os.UserHomeDir reads $HOME on unix and %USERPROFILE%
// on Windows, so a test that only sets HOME silently uses the real home
// dir on Windows. Setting both makes one call correct everywhere; the
// variable the current OS ignores is simply a no-op. The override is
// reverted automatically via t.Setenv's cleanup.
func SetHomeDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
