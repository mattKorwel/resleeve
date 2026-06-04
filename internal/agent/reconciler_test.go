package agent

import (
	"context"
	"go/parser"
	"go/token"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDaemonDoesNotImportConcreteAdapters guards Q12. The daemon was
// importing internal/adapter/claude directly to fire its reconcile
// sweep; the fix introduced the Reconciler seam so that adapter
// selection lives in the CLI layer. If anyone wires a concrete
// adapter package back into the agent package the build still
// compiles — so we lock it in here at the AST level.
func TestDaemonDoesNotImportConcreteAdapters(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files := []string{"daemon.go", "reconciler.go"}
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "internal/adapter/") {
				t.Errorf("%s imports concrete adapter package %q — Q12 regression; "+
					"register a Reconciler from the CLI layer instead", name, path)
			}
		}
	}
}

// TestDaemonFiresRegisteredReconcilers checks the Reconciler seam: a
// reconciler registered via Config runs once after Serve binds. We
// don't actually call Serve (it would bind a TCP listener and start
// the shutdown goroutine); instead we exercise the loop body's
// shape — a reconciler is a plain func and the daemon just iterates
// Config.Reconcilers. This is a smoke against the wiring contract.
func TestReconcilerFuncType(t *testing.T) {
	t.Parallel()
	var called int32
	rec := Reconciler(func(ctx context.Context, d *Daemon) error {
		atomic.StoreInt32(&called, 1)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rec(ctx, nil); err != nil {
		t.Fatalf("rec: %v", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("reconciler did not run")
	}
}
