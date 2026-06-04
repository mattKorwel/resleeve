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
	"github.com/mattkorwel/resleeve/internal/event"
)

// runResume implements `resleeve resume <session-id>` per
// docs/design/round-4/01-conversation-transport.md. Hydrate's the
// target CLI's local state from the captured session, then exec's
// the native resume command so the user lands inside the CLI as if
// they had run it themselves.
//
// Hand-rolled flag parsing because Go's stdlib `flag` package stops
// at the first positional, but flags can appear before or after the
// session id (same shape as scope set / session tail).
func runResume(ctx context.Context, args []string) int {
	var (
		targetCLI   string
		modeStr     string
		cwdOverride string
		printOnly   bool
		sessionID   string
	)

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--cli" && i+1 < len(args):
			targetCLI = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--cli="):
			targetCLI = strings.TrimPrefix(a, "--cli=")
			i++
		case a == "--mode" && i+1 < len(args):
			modeStr = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--mode="):
			modeStr = strings.TrimPrefix(a, "--mode=")
			i++
		case a == "--cwd" && i+1 < len(args):
			cwdOverride = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--cwd="):
			cwdOverride = strings.TrimPrefix(a, "--cwd=")
			i++
		case a == "--print":
			printOnly = true
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "resume: unknown flag %q\n", a)
			resumeUsage(os.Stderr)
			return 2
		default:
			if sessionID != "" {
				fmt.Fprintln(os.Stderr, "resume: only one <session-id> may be given")
				resumeUsage(os.Stderr)
				return 2
			}
			sessionID = a
			i++
		}
	}

	if sessionID == "" {
		resumeUsage(os.Stderr)
		return 2
	}

	mode := adapter.RenderModeAuto
	switch modeStr {
	case "", "auto":
		// default
	case "replay":
		mode = adapter.RenderModeReplay
	case "prime":
		mode = adapter.RenderModePrime
	default:
		fmt.Fprintf(os.Stderr, "resume: invalid --mode %q (expected replay or prime)\n", modeStr)
		return 2
	}

	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume:", err)
		return 1
	}

	ses, err := c.GetSession(ctx, sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume: get session:", err)
		return 1
	}

	cliName := targetCLI
	if cliName == "" {
		cliName = ses.CLI
	}
	if cliName == "" {
		cliName = claude.Name
	}

	var a adapter.Adapter
	switch cliName {
	case claude.Name:
		a = claude.New()
	default:
		fmt.Fprintf(os.Stderr, "resume: no adapter registered for cli %q\n", cliName)
		return 1
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
		EventStream: func() ([]event.Event, error) { return loadAllEvents(ctx, c, sessionID) },
	}

	result, err := a.Hydrate(ctx, view, adapter.HydrateOpts{Mode: mode, Cwd: cwdOverride})
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume: hydrate:", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "✓ hydrated to %s (%s mode)\n", result.Path, result.Mode)
	for _, note := range result.Notes {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", note)
	}

	cmd, cmdArgs, err := a.NativeResumeCmd(ctx, view, result)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resume: native resume command:", err)
		return 1
	}

	if printOnly {
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
	// Exec replaces our process so the target CLI inherits the tty,
	// signals, and env cleanly. Anything after this line does not run.
	if err := syscall.Exec(cmdPath, append([]string{cmd}, cmdArgs...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "resume: exec:", err)
		return 1
	}
	return 0
}

func resumeUsage(w *os.File) {
	fmt.Fprintln(w, "usage: resleeve resume <session-id> [--cli NAME] [--mode replay|prime] [--cwd DIR] [--print]")
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
