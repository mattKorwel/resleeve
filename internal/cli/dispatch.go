package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Version and BuildSHA are overridden at build time via -ldflags. Defaults to
// dev placeholders. Release builds inject the real values:
//
//	go build -ldflags="-X github.com/mattkorwel/resleeve/internal/cli.Version=0.2.0 \
//	                   -X github.com/mattkorwel/resleeve/internal/cli.BuildSHA=abc1234" \
//	         ./cmd/resleeve
//
// See docs/RELEASING.md.
var (
	Version  = "0.0.0-dev"
	BuildSHA = "dev"
)

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
		fmt.Printf("resleeve %s (%s)\n", Version, BuildSHA)
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
	case "scope":
		return runScope(ctx, rest)
	case "plan":
		return runPlan(ctx, rest)
	case "learning":
		return runLearning(ctx, rest)
	case "context":
		return runContext(ctx, rest)
	case "resume":
		return runResume(ctx, rest)
	case "serve":
		return runServe(ctx, rest)
	case "sync":
		return runSync(ctx, rest)
	case "mcp":
		return runMCP(ctx, rest)
	case "register":
		return runRegister(ctx, rest)
	case "login":
		return runLogin(ctx, rest)
	case "logout":
		return runLogout(ctx, rest)
	case "pair":
		return runPair(ctx, rest)
	case "migrate-key":
		return runMigrateKey(ctx, rest)
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
	fmt.Fprintln(w, "  install-bridge   install hooks (and, with --mcp, the resleeve MCP memory server) into the CLI's config")
	fmt.Fprintln(w, "  session          list/show/search/tail captured sessions")
	fmt.Fprintln(w, "  up               start the daemon and install bridges")
	fmt.Fprintln(w, "  down             stop the daemon and remove bridges")
	fmt.Fprintln(w, "  doctor           print daemon + bridge + CLI status")
	fmt.Fprintln(w, "  usage            show data-dir size and session count")
	fmt.Fprintln(w, "  purge            wipe data dir (interactive confirm)")
	fmt.Fprintln(w, "  scope            create/get/list/delete scope tree entries")
	fmt.Fprintln(w, "  plan             write/read/list plans (per scope, named slots)")
	fmt.Fprintln(w, "  learning         append/list per-scope learnings")
	fmt.Fprintln(w, "  context          print the rolled-up bridge-injectable context for a scope")
	fmt.Fprintln(w, "  resume           re-sleeve a captured session into a fresh CLI instance")
	fmt.Fprintln(w, "  serve            run the v2 sync HTTP server")
	fmt.Fprintln(w, "  sync             manual push/pull escape hatch (requires upstream)")
	fmt.Fprintln(w, "  mcp              run the MCP server (stdio JSON-RPC) for in-agent memory curation")
	fmt.Fprintln(w, "  register         create an account on a `resleeve serve` upstream")
	fmt.Fprintln(w, "  login            unlock the local daemon with your master password")
	fmt.Fprintln(w, "  logout           revoke this device + lock the local daemon")
	fmt.Fprintln(w, "  pair             pair invite / pair accept (multi-device onboarding)")
	fmt.Fprintln(w, "  migrate-key      re-encrypt upstream rows from legacy seal.key to the master-password KEK")
	fmt.Fprintln(w, "  help             show this help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Other commands ship in later stages — see docs/design/round-4/")
}
