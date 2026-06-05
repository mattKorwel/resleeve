package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
	"github.com/mattkorwel/resleeve/internal/testutil"
)

func TestOpencode_DetectReturnsInstalledFalseWhenMissing(t *testing.T) {
	// Force an empty PATH so opencode is guaranteed not found regardless
	// of what's on the host.
	t.Setenv("PATH", t.TempDir())
	a := New()
	d, err := a.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Installed {
		t.Errorf("expected Installed=false with empty PATH, got %+v", d)
	}
}

func TestOpencode_DetectReturnsInstalledTrueWhenPresent(t *testing.T) {
	bin := t.TempDir()
	// On Windows exec.LookPath only resolves names carrying a PATHEXT
	// extension (.exe/.bat/...), so the fake binary needs ".exe".
	name := "opencode"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(bin, name)
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	a := New()
	d, err := a.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !d.Installed {
		t.Errorf("expected Installed=true with binary present, got %+v", d)
	}
	if d.Path != target {
		t.Errorf("Path: got %q, want %q", d.Path, target)
	}
}

func TestOpencode_ToNativeReplayModeErrors(t *testing.T) {
	a := New()
	_, err := a.ToNative(context.Background(), nil, adapter.RenderModeReplay)
	if err == nil {
		t.Fatal("expected error for replay mode")
	}
	if !strings.Contains(err.Error(), "replay") {
		t.Errorf("error message should mention replay mode: %v", err)
	}
}

func TestOpencode_ToNativeAutoPicksPrime(t *testing.T) {
	a := New()
	events := []event.Event{
		{
			EventUUID: "E1",
			SessionID: "S1",
			Seq:       1,
			Timestamp: time.Now(),
			Kind:      event.KindUserMessage,
			Content:   json.RawMessage(`{"text":"hi"}`),
		},
	}
	body, err := a.ToNative(context.Background(), events, adapter.RenderModeAuto)
	if err != nil {
		t.Fatalf("ToNative auto: %v", err)
	}
	if !strings.Contains(string(body), "# Resumed session:") {
		t.Errorf("auto mode should have produced prime markdown: %q", body)
	}
}

func TestOpencode_HydratePrimeWritesScratchFile(t *testing.T) {
	home := t.TempDir()
	testutil.SetHomeDir(t, home)

	a := New()
	session := adapter.SessionView{
		SessionID: "S1",
		CLI:       "claude",
		Cwd:       "/x",
		EventStream: func() ([]event.Event, error) {
			return []event.Event{
				{
					EventUUID: "E1",
					SessionID: "S1",
					Seq:       1,
					Timestamp: time.Now(),
					Kind:      event.KindUserMessage,
					Content:   json.RawMessage(`{"text":"port this to opencode"}`),
				},
			}, nil
		},
	}
	result, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{})
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if result.Mode != adapter.RenderModePrime {
		t.Errorf("Mode: got %q, want %q", result.Mode, adapter.RenderModePrime)
	}
	if result.SessionID == "" || result.SessionID == "S1" {
		t.Errorf("expected freshly minted SessionID, got %q", result.SessionID)
	}
	wantDir := filepath.Join(home, ".resleeve", "hydrate")
	if !strings.HasPrefix(result.Path, wantDir+string(filepath.Separator)) {
		t.Errorf("Path %q not under %q", result.Path, wantDir)
	}
	body, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(body), "port this to opencode") {
		t.Errorf("scratch file missing user message: %q", string(body))
	}
}

func TestOpencode_HydrateReplayRejected(t *testing.T) {
	testutil.SetHomeDir(t, t.TempDir())
	a := New()
	session := adapter.SessionView{
		SessionID:   "S1",
		EventStream: func() ([]event.Event, error) { return nil, nil },
	}
	_, err := a.Hydrate(context.Background(), session, adapter.HydrateOpts{Mode: adapter.RenderModeReplay})
	if err == nil {
		t.Fatal("expected error for replay mode")
	}
}

func TestOpencode_NativeResumeCmdReturnsPromptFile(t *testing.T) {
	a := New()
	cmd, args, err := a.NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModePrime, Path: "/tmp/x.md"},
	)
	if err != nil {
		t.Fatalf("NativeResumeCmd: %v", err)
	}
	if cmd != "opencode" {
		t.Errorf("cmd: got %q, want %q", cmd, "opencode")
	}
	if len(args) != 2 || args[0] != "--prompt-file" || args[1] != "/tmp/x.md" {
		t.Errorf("args: got %#v, want [--prompt-file /tmp/x.md]", args)
	}
}

func TestOpencode_NativeResumeCmdRejectsReplayResult(t *testing.T) {
	a := New()
	_, _, err := a.NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "S1"},
		adapter.HydrateResult{Mode: adapter.RenderModeReplay, Path: "/tmp/x.jsonl"},
	)
	if err == nil {
		t.Fatal("expected error for replay-mode result")
	}
}

func TestOpencode_CaptureVerbsReturnNotImplemented(t *testing.T) {
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{}); !errors.Is(err, adapter.ErrNotImplemented) {
		t.Errorf("InstallBridge: want ErrNotImplemented, got %v", err)
	}
	if err := a.UninstallBridge(context.Background()); !errors.Is(err, adapter.ErrNotImplemented) {
		t.Errorf("UninstallBridge: want ErrNotImplemented, got %v", err)
	}
	if _, err := a.FromNative(context.Background(), nil, adapter.Source{}); !errors.Is(err, adapter.ErrNotImplemented) {
		t.Errorf("FromNative: want ErrNotImplemented, got %v", err)
	}
}
