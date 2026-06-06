//go:build unix

package codex

import "fmt"

// primeResumeCmd builds the prime-mode resume command on unix: `sh -c` so
// we can stdin-redirect the synthesized prompt file into `codex exec -`.
// %q quotes the path so spaces in the hydrate dir survive the round-trip.
func primeResumeCmd(path string) (string, []string) {
	inner := fmt.Sprintf("codex exec - < %q", path)
	return "sh", []string{"-c", inner}
}
