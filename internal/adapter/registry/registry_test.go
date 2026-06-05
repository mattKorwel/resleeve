package registry_test

import (
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/registry"

	// Blank-import claude so its init() registers, mirroring how the CLI
	// wires adapters (internal/cli/adapters.go). Other adapters register
	// the same way and are covered in their own packages.
	_ "github.com/mattkorwel/resleeve/internal/adapter/claude"
)

func TestRegisteredAdapterResolves(t *testing.T) {
	a, ok := registry.New("claude")
	if !ok {
		t.Fatal(`registry.New("claude"): not registered`)
	}
	if a.Name() != "claude" {
		t.Fatalf("Name() = %q, want claude", a.Name())
	}
}

func TestUnknownAdapter(t *testing.T) {
	if a, ok := registry.New("does-not-exist"); ok || a != nil {
		t.Fatalf("registry.New(unknown) = (%v, %v), want (nil, false)", a, ok)
	}
}

func TestNamesIncludesClaude(t *testing.T) {
	found := false
	for _, n := range registry.Names() {
		if n == "claude" {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry.Names() missing claude: %v", registry.Names())
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	registry.Register("claude", func() adapter.Adapter { return nil })
}
