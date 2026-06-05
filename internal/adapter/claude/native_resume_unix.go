//go:build unix

package claude

import "fmt"

// primeResumeCmd builds the prime-mode resume command on unix: `sh -c` so
// we can stdin-redirect the synthesized prompt file. %q quotes the path so
// spaces in the hydrate dir (rare but possible) survive the round-trip.
func primeResumeCmd(path string) (string, []string) {
	inner := fmt.Sprintf("claude < %q", path)
	return "sh", []string{"-c", inner}
}
