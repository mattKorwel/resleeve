package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/mattkorwel/resleeve/internal/mcp"
)

// runMCP runs the resleeve MCP server. By default it speaks JSON-RPC
// 2.0 over stdin/stdout (the transport every MCP host uses for local
// servers — Claude Code, opencode, codex, gemini-cli). Install via
// the host's own command, e.g.:
//
//	claude mcp add resleeve resleeve mcp
//
// Scope resolution order (matches internal/adapter/claude.deriveScope
// so the MCP server and the SessionStart hook agree on which scope's
// context to auto-load):
//
//  1. --scope flag (explicit override)
//  2. $RESLEEVE_SCOPE env var
//  3. .resleeve-scope marker walked up from cwd
//  4. filepath.Base(cwd)
//
// stdin/stdout warning: in stdio mode stdout is the JSON-RPC channel.
// We deliberately route the verb's own log output through stderr via
// mcp.StderrLogger so library code stays neutral.
func runMCP(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	scope := fs.String("scope", "", "scope to auto-load context for (overrides $RESLEEVE_SCOPE / marker)")
	stdio := fs.Bool("stdio", true, "speak JSON-RPC over stdin/stdout (the only transport for now)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*stdio {
		// HTTP transport reserved for a future debug surface; the
		// in-process MCP servers every host currently runs use stdio.
		fmt.Fprintln(os.Stderr, "resleeve mcp: only --stdio is supported in this version")
		return 2
	}

	c, err := clientFromEndpoint()
	if err != nil {
		// Match the F14 lesson: don't be silent. Print to stderr so
		// the operator sees it; the MCP host typically surfaces this
		// in its server-startup log.
		fmt.Fprintln(os.Stderr, "resleeve mcp: no daemon endpoint —", err)
		fmt.Fprintln(os.Stderr, "  start the daemon with `resleeve up` first.")
		return 1
	}

	// Resolve default scope once at boot. Tools fall back to this
	// when a call doesn't specify scope; buildInstructions uses it
	// to populate the "Auto-loaded scope context" section.
	defaultScope := *scope
	if defaultScope == "" {
		cwd, _ := os.Getwd()
		defaultScope = mcp.ResolveScope(cwd)
	}

	srv := mcp.New(mcp.Config{
		Client:       c,
		DefaultScope: defaultScope,
		Logger:       mcp.StderrLogger,
	})

	if err := srv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "resleeve mcp:", err)
		return 1
	}
	return 0
}
