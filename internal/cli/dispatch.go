package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Version is overridden at build time via -ldflags. Defaults to dev.
var Version = "0.0.0-dev"

// Main is the CLI entry point. Returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	verb, rest := args[0], args[1:]
	switch verb {
	case "version", "--version", "-v":
		fmt.Println("resleeve", Version)
		return 0
	case "agent":
		return runAgent(ctx, rest)
	case "hook":
		return runHook(ctx, rest)
	case "install-bridge":
		return runInstallBridge(ctx, rest)
	case "session":
		return runSession(ctx, rest)
	case "up":
		return runUp(ctx, rest)
	case "down":
		return runDown(ctx, rest)
	case "doctor":
		return runDoctor(ctx, rest)
	case "purge":
		return runPurge(ctx, rest)
	case "usage":
		return runUsage(ctx, rest)
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "resleeve: unknown command %q\n\n", verb)
		printUsage(os.Stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: resleeve <command> [args]")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  version          print version and exit")
	fmt.Fprintln(w, "  agent            run the local daemon")
	fmt.Fprintln(w, "  hook             process a hook event (stdin → daemon)")
	fmt.Fprintln(w, "  install-bridge   install hooks into the harness's settings file")
	fmt.Fprintln(w, "  session          list/show/search/tail captured sessions")
	fmt.Fprintln(w, "  up               start the daemon and install bridges")
	fmt.Fprintln(w, "  down             stop the daemon and remove bridges")
	fmt.Fprintln(w, "  doctor           print daemon + bridge + CLI status")
	fmt.Fprintln(w, "  usage            show data-dir size and session count")
	fmt.Fprintln(w, "  purge            wipe data dir (interactive confirm)")
	fmt.Fprintln(w, "  help             show this help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Other commands ship in later stages — see docs/design/round-3/00-v1-cut.md")
}
