//go:build windows

package claude

import "fmt"

// primeResumeCmd builds the prime-mode resume command on Windows: `cmd /c`,
// which honors `<` stdin redirection of the synthesized prompt file. The
// path is wrapped in plain double quotes (not %q) so spaces survive while
// backslashes stay literal — cmd.exe does not unescape `\\`.
func primeResumeCmd(path string) (string, []string) {
	inner := fmt.Sprintf(`claude < "%s"`, path)
	return "cmd", []string{"/c", inner}
}
