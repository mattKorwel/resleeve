package opencode

import (
	"context"
	"fmt"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// NativeResumeCmd returns the command + args that resume the hydrated
// session natively.
//
// Replay mode: `opencode run --session <id>`. Hydrate already ran
// `opencode import`, which preserves the provided session id, so resuming
// by id picks up the freshly materialized native session.
//
// Prime mode: `opencode run "<prompt>"`. opencode has no --prompt flag;
// the synthesized prompt is passed positionally (verified cli/cmd/run.ts).
// The prompt body lives at result.Path; the caller (cli) reads the file
// and substitutes its contents for the placeholder, OR runs it via stdin
// for very large prompts. We return the path as the positional arg so the
// command is well-formed; cli is responsible for inlining the contents.
func (a *Adapter) NativeResumeCmd(ctx context.Context, session adapter.SessionView, result adapter.HydrateResult) (string, []string, error) {
	switch result.Mode {
	case adapter.RenderModeReplay:
		sid := result.SessionID
		if strings.TrimSpace(sid) == "" {
			sid = session.SessionID
		}
		if strings.TrimSpace(sid) == "" {
			return "", nil, fmt.Errorf("opencode.NativeResumeCmd: replay result missing SessionID")
		}
		return "opencode", []string{"run", "--session", sid}, nil
	case adapter.RenderModePrime:
		if strings.TrimSpace(result.Path) == "" {
			return "", nil, fmt.Errorf("opencode.NativeResumeCmd: prime result missing Path")
		}
		// Positional prompt. The cli layer inlines result.Path's contents
		// (or pipes via stdin for large prompts); we hand back the path so
		// the command shape is unambiguous.
		return "opencode", []string{"run", result.Path}, nil
	default:
		return "", nil, fmt.Errorf("opencode.NativeResumeCmd: hydrate result has unknown mode %q", result.Mode)
	}
}
