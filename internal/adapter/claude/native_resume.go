package claude

import (
	"context"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// NativeResumeCmd returns the shell command + args that resumes the
// hydrated session natively.
//
// Replay mode: `claude --resume <sid>` — CC's own resume machinery
// takes over from the JSONL Hydrate just wrote.
//
// Prime mode (claude→claude with --mode prime): the synthesized
// prompt lives at result.Path. Claude Code's CLI doesn't currently
// accept a prompt file directly; we shell-redirect the file as stdin
// (via the platform shell — see primeResumeCmd). This is a stopgap
// until CC exposes a `--prompt-file` flag — the seam is here so we can
// swap the args without changing callers.
func (a *Adapter) NativeResumeCmd(ctx context.Context, session adapter.SessionView, result adapter.HydrateResult) (string, []string, error) {
	switch result.Mode {
	case adapter.RenderModeReplay:
		return "claude", []string{"--resume", session.SessionID}, nil
	case adapter.RenderModePrime:
		if result.Path == "" {
			return "", nil, fmt.Errorf("claude.NativeResumeCmd: prime result missing Path")
		}
		cmd, args := primeResumeCmd(result.Path)
		return cmd, args, nil
	default:
		return "", nil, fmt.Errorf("claude.NativeResumeCmd: hydrate result has unknown mode %q", result.Mode)
	}
}
