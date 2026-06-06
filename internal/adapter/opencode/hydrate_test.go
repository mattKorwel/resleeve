package opencode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
	"github.com/mattkorwel/resleeve/internal/testutil"
)

// stubImport replaces execCommand with one that records the invocation and
// either succeeds or fails. It restores the original on cleanup.
func stubImport(t *testing.T, fail bool, captured *[]string) {
	t.Helper()
	orig := execCommand
	t.Cleanup(func() { execCommand = orig })
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if captured != nil {
			*captured = append([]string{name}, args...)
		}
		if fail {
			// `false` exits non-zero on every platform we target except
			// Windows; use a command guaranteed to fail there.
			return exec.CommandContext(ctx, "this-command-does-not-exist-xyz")
		}
		// `true` is a no-op success on unix; on Windows use `cmd /c exit 0`.
		return exec.CommandContext(ctx, "true")
	}
}

func replaySession(t *testing.T) adapter.SessionView {
	t.Helper()
	return adapter.SessionView{
		SessionID: "ses_1",
		CLI:       Name,
		Cwd:       "/work/p",
		EventStream: func() ([]event.Event, error) {
			return captureEvents(), nil
		},
	}
}

func TestHydrate_ReplayWritesImportAndRunsImport(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("`true` not available on this platform")
	}
	home := t.TempDir()
	testutil.SetHomeDir(t, home)

	var captured []string
	stubImport(t, false, &captured)

	res, err := New().Hydrate(context.Background(), replaySession(t), adapter.HydrateOpts{})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if res.Mode != adapter.RenderModeReplay {
		t.Errorf("Mode: got %q, want replay", res.Mode)
	}
	if res.SessionID != "ses_1" {
		t.Errorf("SessionID: got %q, want ses_1 (import preserves id)", res.SessionID)
	}
	if !strings.HasSuffix(res.Path, "ses_1.json") {
		t.Errorf("Path: got %q, want .../ses_1.json", res.Path)
	}
	// `opencode import <path>` must have been invoked.
	if len(captured) != 3 || captured[0] != "opencode" || captured[1] != "import" || captured[2] != res.Path {
		t.Errorf("import invocation: got %v, want [opencode import %s]", captured, res.Path)
	}
	// The written file must be valid import JSON.
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read import file: %v", err)
	}
	var f ImportFile
	if err := json.Unmarshal(body, &f); err != nil {
		t.Errorf("written file is not import JSON: %v", err)
	}
}

func TestHydrate_ReplayImportFailureFallsBackToPrime(t *testing.T) {
	home := t.TempDir()
	testutil.SetHomeDir(t, home)

	stubImport(t, true, nil)

	res, err := New().Hydrate(context.Background(), replaySession(t), adapter.HydrateOpts{})
	if err != nil {
		t.Fatalf("Hydrate (expected prime fallback): %v", err)
	}
	if res.Mode != adapter.RenderModePrime {
		t.Errorf("Mode: got %q, want prime (fallback)", res.Mode)
	}
	if !strings.HasSuffix(res.Path, ".md") {
		t.Errorf("prime fallback Path: got %q, want .md", res.Path)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "import failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a fallback note mentioning import failure, got %v", res.Notes)
	}
}

func TestHydrate_PrimeMode(t *testing.T) {
	home := t.TempDir()
	testutil.SetHomeDir(t, home)

	session := adapter.SessionView{
		SessionID: "ses_1",
		CLI:       "claude",
		Cwd:       "/x",
		EventStream: func() ([]event.Event, error) {
			return []event.Event{{
				EventUUID: "e1",
				SessionID: "ses_1",
				Seq:       1,
				Timestamp: time.Now(),
				Kind:      event.KindUserMessage,
				Content:   json.RawMessage(`{"text":"port this to opencode"}`),
			}}, nil
		},
	}
	res, err := New().Hydrate(context.Background(), session, adapter.HydrateOpts{Mode: adapter.RenderModePrime})
	if err != nil {
		t.Fatalf("Hydrate prime: %v", err)
	}
	if res.Mode != adapter.RenderModePrime {
		t.Errorf("Mode: got %q, want prime", res.Mode)
	}
	if res.SessionID == "" || res.SessionID == "ses_1" {
		t.Errorf("expected freshly minted SessionID, got %q", res.SessionID)
	}
	wantDir := filepath.Join(home, ".resleeve", "hydrate")
	if !strings.HasPrefix(res.Path, wantDir+string(filepath.Separator)) {
		t.Errorf("Path %q not under %q", res.Path, wantDir)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read prime file: %v", err)
	}
	if !strings.Contains(string(body), "port this to opencode") {
		t.Errorf("prime file missing user message: %q", body)
	}
}

func TestHydrate_NoEventStreamErrors(t *testing.T) {
	testutil.SetHomeDir(t, t.TempDir())
	_, err := New().Hydrate(context.Background(), adapter.SessionView{SessionID: "s"}, adapter.HydrateOpts{})
	if err == nil {
		t.Fatal("expected error when EventStream is nil")
	}
}
