package claude

import (
	"context"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// NativeResumeCmd returns the shell command + args that resumes the
// hydrated session natively. For replay mode, this is `claude --resume
// <sid>` — CC's own resume machinery takes over from the JSONL Hydrate
// just wrote. Prime mode lands in a follow-up commit (will return a
// `--prompt-file` style command for the target CLI).
func (a *Adapter) NativeResumeCmd(ctx context.Context, session adapter.SessionView, result adapter.HydrateResult) (string, []string, error) {
	switch result.Mode {
	case adapter.RenderModeReplay:
		return "claude", []string{"--resume", session.SessionID}, nil
	case adapter.RenderModePrime:
		return "", nil, fmt.Errorf("%w: claude.NativeResumeCmd prime mode", adapter.ErrNotImplemented)
	default:
		return "", nil, fmt.Errorf("claude.NativeResumeCmd: hydrate result has unknown mode %q", result.Mode)
	}
}
