package antigravity

import (
	"context"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// NativeResumeCmd returns the shell command + args that resumes the hydrated
// session natively.
//
//   - Same-CLI resume (the session id is a real antigravity conversation id):
//     the Hydrate result will be in REPLAY mode only if a caller forced it
//     (unsupported — Hydrate errors first), so in practice same-CLI resume is
//     issued by passing the original conversation id and calling this with a
//     replay-shaped result OR directly. We honor the native resume path:
//     `agy --conversation <id>`.
//
//   - Prime mode (cross-CLI target, or explicit --mode prime): the
//     synthesized prompt lives at result.Path. agy's --print/-p takes a single
//     non-interactive prompt; we feed the file's contents via the platform
//     shell (see primeResumeCmd) so the seam stays swappable if agy gains a
//     --prompt-file flag.
func (a *Adapter) NativeResumeCmd(ctx context.Context, session adapter.SessionView, result adapter.HydrateResult) (string, []string, error) {
	switch result.Mode {
	case adapter.RenderModeReplay:
		// Native same-CLI resume: hand off to agy's own conversation store.
		if session.SessionID == "" {
			return "", nil, fmt.Errorf("antigravity.NativeResumeCmd: replay resume needs a conversation id")
		}
		return binary, []string{"--conversation", session.SessionID}, nil
	case adapter.RenderModePrime:
		if result.Path == "" {
			return "", nil, fmt.Errorf("antigravity.NativeResumeCmd: prime result missing Path")
		}
		cmd, args := primeResumeCmd(result.Path)
		return cmd, args, nil
	default:
		return "", nil, fmt.Errorf("antigravity.NativeResumeCmd: hydrate result has unknown mode %q", result.Mode)
	}
}

// NativeResumeByConversationID returns the native command that reopens an
// antigravity-origin conversation directly, bypassing Hydrate. This is the
// recommended same-CLI resume path (full fidelity via agy's own store).
func (a *Adapter) NativeResumeByConversationID(conversationID string) (string, []string, error) {
	if conversationID == "" {
		return "", nil, fmt.Errorf("antigravity: empty conversation id")
	}
	return binary, []string{"--conversation", conversationID}, nil
}
