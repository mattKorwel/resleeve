package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// runInstallBridge is the "resleeve install-bridge" subcommand. Writes
// the bridge plugin (hook entries pointing at this resleeve binary) into
// the harness's settings file. Idempotent.
func runInstallBridge(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("install-bridge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	adapterName := fs.String("adapter", "claude", "adapter name")
	dryRun := fs.Bool("dry-run", false, "print resulting settings; don't write")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	a, err := pickAdapter(*adapterName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install-bridge:", err)
		return 1
	}

	if err := a.InstallBridge(ctx, adapter.InstallOpts{DryRun: *dryRun}); err != nil {
		fmt.Fprintln(os.Stderr, "install-bridge:", err)
		return 1
	}
	if !*dryRun {
		fmt.Println("Installed", *adapterName, "bridge.")
	}
	return 0
}
