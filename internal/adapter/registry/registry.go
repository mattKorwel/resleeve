// Package registry is the single lookup table mapping a CLI adapter name
// (e.g. "claude", "opencode", "codex") to a factory that constructs it.
//
// Before round 9 the CLI layer hand-maintained two parallel switch
// statements (cli/hook.go pickAdapter + cli/resume.go hydrateForCLI). As
// adapters multiplied that drifted. Now each adapter package registers
// itself from an init() — see e.g. internal/adapter/claude/adapter.go —
// and the CLI imports this registry plus a set of blank adapter imports
// (internal/cli/adapters.go) so the init()s run.
//
// The registry intentionally only knows the Adapter interface; it has no
// dependency on any concrete adapter package, so there is no import
// cycle (adapter packages import registry, never the reverse).
package registry

import (
	"sort"
	"sync"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// Factory constructs a fresh adapter. Adapters are cheap, stateless
// structs, so a new one per call is fine (matches the old *.New() calls).
type Factory func() adapter.Adapter

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register records a factory under name. Called from adapter package
// init()s. Registering the same name twice panics — that's a build-time
// programming error (two adapters claiming one name), not a runtime
// condition to tolerate.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("adapter/registry: duplicate registration for " + name)
	}
	factories[name] = f
}

// New constructs the adapter registered under name. The bool is false
// when no adapter is registered for that name (the caller decides how to
// report it — the CLI prints an "unknown adapter" / "no adapter for cli"
// message matching the old switch defaults).
func New(name string) (adapter.Adapter, bool) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, false
	}
	return f(), true
}

// Names returns the sorted list of registered adapter names, for help
// text and diagnostics.
func Names() []string {
	mu.RLock()
	out := make([]string, 0, len(factories))
	for name := range factories {
		out = append(out, name)
	}
	mu.RUnlock()
	sort.Strings(out)
	return out
}
