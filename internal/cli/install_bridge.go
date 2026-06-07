package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// runInstallBridge is the "resleeve install-bridge" subcommand. Writes the
// bridge plugin (hook entries pointing at this resleeve binary) into the
// harness's settings file. Idempotent.
//
// Flags:
//
//	--mcp        ALSO register the `resleeve mcp` memory server in the
//	             CLI's MCP config (in addition to the capture hook).
//	--mcp-only   register ONLY the MCP server; skip the capture hook
//	             entirely (the memory-pull path for CLIs that can't be
//	             push-injected via a hook, e.g. opencode / antigravity).
//	--uninstall  remove resleeve's entries instead of installing them
//	             (honors --mcp / --mcp-only to scope what is removed).
func runInstallBridge(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("install-bridge", flag.ContinueOnError)
	memoryOnly := fs.Bool("memory-only", false, "install only the SessionStart hook (memory injection only — no session/tool/event capture)")
	fs.SetOutput(os.Stderr)
	adapterName := fs.String("adapter", "claude", "adapter name")
	dryRun := fs.Bool("dry-run", false, "print resulting settings; don't write")
	mcp := fs.Bool("mcp", false, "also register the `resleeve mcp` memory server in the CLI's MCP config")
	mcpOnly := fs.Bool("mcp-only", false, "register ONLY the MCP memory server; skip the capture hook")
	uninstall := fs.Bool("uninstall", false, "remove resleeve's entries instead of installing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	a, err := pickAdapter(*adapterName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install-bridge:", err)
		return 1
	}

	// --mcp-only implies MCP work and suppresses the hook.
	doHook := !*mcpOnly
	doMCP := *mcp || *mcpOnly

	opts := adapter.InstallOpts{DryRun: *dryRun, MemoryOnly: *memoryOnly}

	if *uninstall {
		if doHook {
			if err := a.UninstallBridge(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "install-bridge:", err)
				return 1
			}
		}
		if doMCP {
			if err := a.UninstallMCP(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "install-bridge:", err)
				return 1
			}
		}
		if !*dryRun {
			fmt.Println("Removed", *adapterName, summarize(doHook, doMCP), "entries.")
		}
		return 0
	}

	if doHook {
		if err := a.InstallBridge(ctx, opts); err != nil {
			fmt.Fprintln(os.Stderr, "install-bridge:", err)
			return 1
		}
	}
	if doMCP {
		if err := a.InstallMCP(ctx, opts); err != nil {
			fmt.Fprintln(os.Stderr, "install-bridge:", err)
			return 1
		}
	}
	if !*dryRun {
		fmt.Println("Installed", *adapterName, summarize(doHook, doMCP)+".")
	}
	return 0
}

// summarize renders a short noun for the install/uninstall confirmation
// line based on which capabilities were touched.
func summarize(hook, mcp bool) string {
	switch {
	case hook && mcp:
		return "bridge + mcp"
	case mcp:
		return "mcp"
	default:
		return "bridge"
	}
}
