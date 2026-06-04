package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	"github.com/mattkorwel/resleeve/internal/adapter/opencode"
	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// resumeOpts is the parsed result of the resume verb's argv.
type resumeOpts struct {
	targetCLI   string
	mode        adapter.RenderMode
	cwdOverride string
	printOnly   bool
	noPull      bool
	sessionID   string
}

// runResume implements `resleeve resume <session-id>` per
// docs/design/round-4/01-conversation-transport.md. Hydrate's the
// target CLI's local state from the captured session, then exec's
// the native resume command so the user lands inside the CLI as if
// they had run it themselves.
//
// Split (Q4) into:
//   - parseResumeArgs   — flag parsing
//   - resolveSession    — daemon client + GetSession (with on-demand pull)
//   - hydrateForCLI     — adapter selection + Hydrate (incl. prime PlanContent fetch)
//   - execOrPrint       — print-only formatting vs syscall.Exec hand-off
func runResume(ctx context.Context, args []string) int {
	opts, code := parseResumeArgs(args)
	if code != 0 {
		return code
	}

	c, ses, code := resolveSession(ctx, opts)
	if code != 0 {
		return code
	}

	a, view, result, code := hydrateForCLI(ctx, c, ses, opts)
	if code != 0 {
		return code
	}

	return execOrPrint(ctx, a, view, result, opts)
}

// parseResumeArgs is intentionally hand-rolled: stdlib `flag` stops at
// the first positional, but `resleeve resume` accepts flags before AND
// after the session id (same shape as `scope set`, `session tail`).
// Returns a non-zero exit code on usage error.
func parseResumeArgs(args []string) (resumeOpts, int) {
	var (
		opts    resumeOpts
		modeStr string
	)

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--cli" && i+1 < len(args):
			opts.targetCLI = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--cli="):
			opts.targetCLI = strings.TrimPrefix(a, "--cli=")
			i++
		case a == "--mode" && i+1 < len(args):
			modeStr = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--mode="):
			modeStr = strings.TrimPrefix(a, "--mode=")
			i++
		case a == "--cwd" && i+1 < len(args):
			opts.cwdOverride = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--cwd="):
			opts.cwdOverride = strings.TrimPrefix(a, "--cwd=")
			i++
		case a == "--print":
			opts.printOnly = true
			i++
		case a == "--no-pull":
			opts.noPull = true
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "resume: unknown flag %q\n", a)
			resumeUsage(os.Stderr)
			return opts, 2
		default:
			if opts.sessionID != "" {
				fmt.Fprintln(os.Stderr, "resume: only one <session-id> may be given")
				resumeUsage(os.Stderr)
				return opts, 2
			}
			opts.sessionID = a
			i++
		}
	}

	if opts.sessionID == "" {
		resumeUsage(os.Stderr)
		return opts, 2
	}

	switch modeStr {
	case "", "auto":
		opts.mode = adapter.RenderModeAuto
	case "replay":
		opts.mode = adapter.RenderModeReplay
	case "prime":
		opts.mode = adapter.RenderModePrime
	default:
		fmt.Fprintf(os.Stderr, "resume: invalid --mode %q (expected replay or prime)\n", modeStr)
		return opts, 2
	}

	return opts, 0
}

// resolveSession opens a daemon client, kicks off an opportunistic
// on-demand pull (unless --no-pull was given), and fetches the session
// the caller asked for. Returns the client so the caller can keep using
// it (e.g. for prime-mode PlanContent fetch and pagination of events).
func resolveSession(ctx context.Context, opts resumeOpts) (*agent.Client, *rsql.Session, int) {
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume:", err)
		return nil, nil, 1
	}

	// Best-effort pre-pull so resume reflects the freshest upstream
	// state (e.g., a session captured on another machine seconds ago).
	// Falls back to local state on timeout/unreachable — see
	// docs/design/round-4/03-rehydrate-ux.md §"Failure modes".
	if !opts.noPull {
		tryOnDemandPull(ctx, c, defaultOnDemandPullTimeout, os.Stderr)
	}

	ses, err := c.GetSession(ctx, opts.sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume: get session:", err)
		return nil, nil, 1
	}
	return c, ses, 0
}

// hydrateForCLI picks the right adapter for the session, builds the
// SessionView, fetches prime-mode PlanContent if applicable, and
// dispatches Hydrate. Returns the adapter + view + Hydrate result so
// execOrPrint can build the native-resume command on top.
func hydrateForCLI(ctx context.Context, c *agent.Client, ses *rsql.Session, opts resumeOpts) (adapter.Adapter, adapter.SessionView, *adapter.HydrateResult, int) {
	cliName := opts.targetCLI
	if cliName == "" {
		cliName = ses.CLI
	}
	if cliName == "" {
		cliName = claude.Name
	}

	mode := opts.mode
	var a adapter.Adapter
	switch cliName {
	case claude.Name:
		a = claude.New()
	case opencode.Name:
		a = opencode.New()
		// opencode can only re-sleeve via prime mode (it can't ingest
		// claude's JSONL). If the caller didn't ask for an explicit
		// mode, force prime so Auto doesn't fall through into a
		// replay attempt the adapter would reject anyway.
		if mode == adapter.RenderModeAuto {
			mode = adapter.RenderModePrime
		}
	default:
		fmt.Fprintf(os.Stderr, "resume: no adapter registered for cli %q\n", cliName)
		return nil, adapter.SessionView{}, nil, 1
	}

	view := adapter.SessionView{
		SessionID:   ses.ID,
		CLI:         ses.CLI,
		CLIVersion:  ses.CLIVersion,
		Cwd:         ses.Cwd,
		GitBranch:   ses.GitBranch,
		Model:       ses.Model,
		StartedAt:   ses.StartedAt,
		EndedAt:     ses.EndedAt,
		EventStream: func() ([]event.Event, error) { return loadAllEvents(ctx, c, opts.sessionID) },
	}

	planContent := fetchPrimePlan(ctx, c, ses, mode)

	result, err := a.Hydrate(ctx, view, adapter.HydrateOpts{Mode: mode, Cwd: opts.cwdOverride, PlanContent: planContent})
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume: hydrate:", err)
		return nil, adapter.SessionView{}, nil, 1
	}

	fmt.Fprintf(os.Stderr, "✓ hydrated to %s (%s mode)\n", result.Path, result.Mode)
	for _, note := range result.Notes {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", note)
	}
	return a, view, &result, 0
}

// fetchPrimePlan renders a "## Plan" section from the session's scope's
// rolled-up memory context (item #8 on the polish punch list) by hitting
// the same daemon endpoint the SessionStart hook uses (GET /v1/context).
// Replay mode skips this entirely (the section is prime-only). Empty /
// "unknown" scopes are skipped: BuildContext would have nothing useful
// to return and "unknown" is a sentinel the slot writer uses when the
// source CLI never sent a scope. Failures are non-fatal — prime can
// still render with an empty plan.
func fetchPrimePlan(ctx context.Context, c *agent.Client, ses *rsql.Session, mode adapter.RenderMode) string {
	if mode != adapter.RenderModePrime {
		return ""
	}
	scope := ses.Slot.Scope
	if scope == "" || scope == "unknown" {
		return ""
	}
	ctxBody, ctxErr := c.GetContext(ctx, scope)
	if ctxErr != nil {
		fmt.Fprintf(os.Stderr, "resume: warning: fetch context for scope %q: %v\n", scope, ctxErr)
		return ""
	}
	return ctxBody
}

// execOrPrint either prints the native resume command (--print) or
// syscall.Exec's into it so the target CLI inherits this process's tty,
// signals, and env cleanly. After a successful Exec, nothing in this
// process runs again.
func execOrPrint(ctx context.Context, a adapter.Adapter, view adapter.SessionView, result *adapter.HydrateResult, opts resumeOpts) int {
	cmd, cmdArgs, err := a.NativeResumeCmd(ctx, view, *result)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume: native resume command:", err)
		return 1
	}

	if opts.printOnly {
		// Print in a shell-paste-friendly form.
		fmt.Println(strings.Join(append([]string{cmd}, cmdArgs...), " "))
		return 0
	}

	cmdPath, err := exec.LookPath(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resume: %s not in PATH: %v\n", cmd, err)
		fmt.Fprintf(os.Stderr, "       (re-run with --print to get the command and execute it yourself)\n")
		return 1
	}
	if err := syscall.Exec(cmdPath, append([]string{cmd}, cmdArgs...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "resume: exec:", err)
		return 1
	}
	return 0
}

func resumeUsage(w *os.File) {
	fmt.Fprintln(w, "usage: resleeve resume <session-id> [--cli NAME] [--mode replay|prime] [--cwd DIR] [--print] [--no-pull]")
}

// loadAllEvents paginates through the daemon's ListEvents endpoint to
// collect every event for the session. Pagination uses sinceSeq as an
// exclusive cursor; same-nanosecond tied events at a page boundary are
// vanishingly rare in practice (seq is ts.UnixNano()) and acceptable
// for replay-mode rendering, where order within a tie is determined
// by event_uuid (see ToNative).
func loadAllEvents(ctx context.Context, c eventLister, sessionID string) ([]event.Event, error) {
	const pageSize = 1000
	var all []event.Event
	var since int64 = 0
	for {
		page, err := c.ListEvents(ctx, sessionID, since, pageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		last := page[len(page)-1].Seq
		if last <= since {
			break
		}
		since = last
		if len(page) < pageSize {
			break
		}
	}
	return all, nil
}

// eventLister is the subset of agent.Client that loadAllEvents needs,
// extracted so the function is testable without a live daemon.
type eventLister interface {
	ListEvents(ctx context.Context, sessionID string, sinceSeq int64, limit int) ([]event.Event, error)
}
