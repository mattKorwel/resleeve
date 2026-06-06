//go:build windows

package antigravity

import "fmt"

// primeResumeCmd builds the prime-mode resume command on Windows. cmd.exe has
// no `$(...)` command substitution, so we read the synthesized prompt file
// with `type` and pipe it into agy's stdin via `--print -` is not guaranteed;
// instead we use `cmd /c "type <file> | agy --print"` — agy reads the piped
// prompt. The path is wrapped in plain double quotes (not %q) so spaces
// survive while backslashes stay literal — cmd.exe does not unescape `\\`.
func primeResumeCmd(path string) (string, []string) {
	inner := fmt.Sprintf(`type "%s" | agy --print`, path)
	return "cmd", []string{"/c", inner}
}
