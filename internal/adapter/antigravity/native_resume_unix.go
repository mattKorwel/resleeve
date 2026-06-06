//go:build unix

package antigravity

import "fmt"

// primeResumeCmd builds the prime-mode resume command on unix. agy's
// `--print`/`-p` flag takes a single non-interactive prompt argument; we
// substitute the synthesized prompt file's contents via command
// substitution. `sh -c` lets us do that in one exec. %q quotes the path so
// spaces in the hydrate dir survive the round-trip.
func primeResumeCmd(path string) (string, []string) {
	inner := fmt.Sprintf("agy --print \"$(cat %q)\"", path)
	return "sh", []string{"-c", inner}
}
