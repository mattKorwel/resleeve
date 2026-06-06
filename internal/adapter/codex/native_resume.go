package codex

import (
	"context"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// NativeResumeCmd returns the shell command + args that resumes the
// hydrated session natively.
//
// Replay mode: `codex resume <sessionID>` — Codex resolves the id to the
// rollout file Hydrate just wrote (falling back to a filename UUID walk
// when there's no state-DB entry) and binds to the original recorded cwd.
//
// Prime mode (cross-CLI, or codex→codex with --mode prime): the
// synthesized prompt lives at result.Path. Codex reads a prompt from
// stdin via `codex exec -`; we shell-redirect the file through the
// platform shell (see primeResumeCmd).
func (a *Adapter) NativeResumeCmd(ctx context.Context, session adapter.SessionView, result adapter.HydrateResult) (string, []string, error) {
	switch result.Mode {
	case adapter.RenderModeReplay:
		return "codex", []string{"resume", session.SessionID}, nil
	case adapter.RenderModePrime:
		if result.Path == "" {
			return "", nil, fmt.Errorf("codex.NativeResumeCmd: prime result missing Path")
		}
		cmd, args := primeResumeCmd(result.Path)
		return cmd, args, nil
	default:
		return "", nil, fmt.Errorf("codex.NativeResumeCmd: hydrate result has unknown mode %q", result.Mode)
	}
}
