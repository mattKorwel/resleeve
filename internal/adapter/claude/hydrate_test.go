package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

func TestHydrate_ReplayWritesJSONLToProjectDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	a := New()
	cwd := "/private/tmp/x/proj"
	events := []event.Event{
		{EventUUID: "E1", Seq: 1, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"user","sessionId":"S1","cwd":"/private/tmp/x/proj","message":{"role":"user","content":"hi"}}`)}},
		{EventUUID: "E2", Seq: 2, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"assistant","sessionId":"S1","cwd":"/private/tmp/x/proj","message":{"role":"assistant","content":"hello"}}`)}},
	}
	session := adapter.SessionView{
		SessionID:   "S1",
		CLI:         Name,
		Cwd:         cwd,
		EventStream: func() ([]event.Event, error) { return events, nil },
	}
	result, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if result.Mode != adapter.RenderModeReplay {
		t.Errorf("Mode: got %q, want %q", result.Mode, adapter.RenderModeReplay)
	}
	if result.SessionID != "S1" {
		t.Errorf("SessionID: got %q, want %q", result.SessionID, "S1")
	}

	wantPath := filepath.Join(home, ".claude", "projects", "-private-tmp-x-proj", "S1.jsonl")
	if result.Path != wantPath {
		t.Errorf("Path: got %q, want %q", result.Path, wantPath)
	}
	body, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(body), `"type":"user"`) || !strings.Contains(string(body), `"type":"assistant"`) {
		t.Errorf("written file missing expected lines: %q", string(body))
	}
	if strings.Count(string(body), "\n") != 2 {
		t.Errorf("expected 2 JSONL lines, got: %q", string(body))
	}
}

func TestHydrate_ReplayOverwritesExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Pre-create the target with junk to verify atomic overwrite.
	dir := filepath.Join(home, ".claude", "projects", "-tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "S1.jsonl"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := New()
	session := adapter.SessionView{
		SessionID: "S1",
		CLI:       Name,
		Cwd:       "/tmp",
		EventStream: func() ([]event.Event, error) {
			return []event.Event{{Seq: 1, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"user"}`)}}}, nil
		},
	}
	if _, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{}); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "S1.jsonl"))
	if strings.Contains(string(body), "stale") {
		t.Errorf("stale content survived overwrite: %q", string(body))
	}
}

func TestHydrate_ErrorWhenNoCwdAvailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := New()
	session := adapter.SessionView{
		SessionID:   "S1",
		EventStream: func() ([]event.Event, error) { return nil, nil },
	}
	_, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{})
	if err == nil || !strings.Contains(err.Error(), "no cwd") {
		t.Errorf("expected no-cwd error, got: %v", err)
	}
}

func TestHydrate_CwdOptOverridesSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := New()
	session := adapter.SessionView{
		SessionID:   "S1",
		Cwd:         "/will/be/overridden",
		EventStream: func() ([]event.Event, error) { return nil, nil },
	}
	result, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{Cwd: "/new/cwd"})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	wantPath := filepath.Join(home, ".claude", "projects", "-new-cwd", "S1.jsonl")
	if result.Path != wantPath {
		t.Errorf("override Cwd not used: got %q, want %q", result.Path, wantPath)
	}
}

func TestHydrate_PrimeWritesScratchFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := New()
	session := adapter.SessionView{
		SessionID: "S1",
		CLI:       Name,
		Cwd:       "/x",
		EventStream: func() ([]event.Event, error) {
			return []event.Event{
				{
					EventUUID: "E1",
					SessionID: "S1",
					Seq:       1,
					Kind:      event.KindUserMessage,
					Content:   json.RawMessage(`{"text":"hi"}`),
				},
			}, nil
		},
	}
	result, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{Mode: adapter.RenderModePrime})
	if err != nil {
		t.Fatalf("Hydrate prime: %v", err)
	}
	if result.Mode != adapter.RenderModePrime {
		t.Errorf("Mode: got %q, want %q", result.Mode, adapter.RenderModePrime)
	}
	if result.SessionID == "S1" || result.SessionID == "" {
		t.Errorf("expected freshly minted SessionID, got %q", result.SessionID)
	}
	wantDir := filepath.Join(home, ".resleeve", "hydrate")
	if !strings.HasPrefix(result.Path, wantDir+string(filepath.Separator)) {
		t.Errorf("Path %q not under %q", result.Path, wantDir)
	}
	if !strings.HasSuffix(result.Path, ".md") {
		t.Errorf("Path %q should end in .md", result.Path)
	}
	body, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(body), "# Resumed session:") {
		t.Errorf("scratch file missing prime header: %q", string(body))
	}
}

func TestEncodeCwdForProjectDir(t *testing.T) {
	cases := map[string]string{
		"/Users/x/proj":       "-Users-x-proj",
		"/tmp":                "-tmp",
		"/a/b/c":              "-a-b-c",
		"/Users/x/dev-stuff":  "-Users-x-dev-stuff", // existing hyphens preserved
	}
	for in, want := range cases {
		if got := encodeCwdForProjectDir(in); got != want {
			t.Errorf("encodeCwdForProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNativeResumeCmd_ReplayReturnsClaudeResume(t *testing.T) {
	a := New()
	session := adapter.SessionView{SessionID: "S1"}
	result := adapter.HydrateResult{Mode: adapter.RenderModeReplay, SessionID: "S1"}
	cmd, args, err := a.NativeResumeCmd(context.Background(), session, result)
	if err != nil {
		t.Fatalf("NativeResumeCmd: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("cmd: got %q, want %q", cmd, "claude")
	}
	if len(args) != 2 || args[0] != "--resume" || args[1] != "S1" {
		t.Errorf("args: got %v, want [--resume S1]", args)
	}
}

func TestNativeResumeCmd_PrimeReturnsShellRedirect(t *testing.T) {
	a := New()
	cmd, args, err := a.NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModePrime, Path: "/tmp/x.md"},
	)
	if err != nil {
		t.Fatalf("NativeResumeCmd prime: %v", err)
	}
	if cmd != "sh" {
		t.Errorf("cmd: got %q, want %q", cmd, "sh")
	}
	if len(args) != 2 || args[0] != "-c" || !strings.Contains(args[1], `claude < "/tmp/x.md"`) {
		t.Errorf("args wrong: got %#v", args)
	}
}

func TestNativeResumeCmd_PrimeRejectsMissingPath(t *testing.T) {
	a := New()
	_, _, err := a.NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModePrime},
	)
	if err == nil {
		t.Fatal("expected error for missing Path")
	}
}
