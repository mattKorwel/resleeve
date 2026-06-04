package adapter

import (
	"context"
	"errors"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
)

// ErrNotImplemented is returned by Adapter methods that are part of
// the interface contract but not yet implemented in the current stage.
var ErrNotImplemented = errors.New("adapter: not implemented")

// Adapter is the contract every CLI adapter implements. v1 ships only
// the Claude Code adapter; opencode / Codex / Gemini implementations
// live in v3. See docs/design/round-2/09-adapter-interface.md and
// docs/design/round-4/01-conversation-transport.md for the Stage 5
// re-sleeve verbs.
type Adapter interface {
	// Identity
	Name() string
	Detect(ctx context.Context) (Detection, error)

	// Bridge plugin lifecycle
	InstallBridge(ctx context.Context, opts InstallOpts) error
	UninstallBridge(ctx context.Context) error

	// Capture (translate native input to normalized events)
	FromNative(ctx context.Context, raw []byte, src Source) ([]event.Event, error)

	// Re-sleeve (Stage 5) — see round-4/01-conversation-transport.md.
	ToNative(ctx context.Context, events []event.Event, mode RenderMode) ([]byte, error)
	Hydrate(ctx context.Context, session SessionView, opts HydrateOpts) (HydrateResult, error)
	NativeResumeCmd(ctx context.Context, session SessionView, result HydrateResult) (cmd string, args []string, err error)
}

// Detection describes the state of a CLI's installation on the host.
type Detection struct {
	Installed bool
	Path      string            // absolute path to the CLI binary, if found
	Version   string            // CLI version (vendor-flavored)
	Quirks    map[string]string // adapter-specific notes
}

// InstallOpts configures bridge-plugin installation.
type InstallOpts struct {
	ResleeveBinPath string // absolute path to the resleeve binary (preferred over $PATH lookup)
	Endpoint        string // optional override; daemon endpoint URL
	DryRun          bool   // compute changes, do not write

	// MemoryOnly installs ONLY the SessionStart hook (for additionalContext
	// injection) and adds `--memory-only` to the hook command so the hook
	// skips persisting the SessionStart event to the daemon's events table.
	// PostToolUse / Stop / UserPromptSubmit hooks are not installed at all,
	// so capture is effectively off — memory CRUD via MCP + CLI keeps working
	// (it goes through the scope/plan/learning routes, not events).
	MemoryOnly bool
}

// Source describes where a FromNative input came from.
type Source struct {
	Kind        SourceKind
	SessionHint string // optional session_id when the source knows it
}

// SourceKind discriminates the shape of a FromNative input.
type SourceKind int

const (
	SourceUnknown SourceKind = iota
	SourceHook              // one Claude-Code hook JSON envelope on stdin
	SourceJSONL             // one JSONL line from a session file
)

// SessionView is a read-only projection of a stored session, used by
// Hydrate. EventStream is a lazy closure so callers that only need
// metadata (e.g. a future `resleeve show --remote`) don't have to pay
// for an events load.
type SessionView struct {
	SessionID   string
	CLI         string
	CLIVersion  string
	Cwd         string
	GitBranch   string
	Model       string
	StartedAt   time.Time
	EndedAt     *time.Time
	EventStream func() ([]event.Event, error)
}

// RenderMode discriminates how ToNative / Hydrate emit the target
// CLI's local state. See round-4/01-conversation-transport.md.
type RenderMode string

const (
	// RenderModeAuto lets the adapter pick: replay when source CLI ==
	// target CLI and the adapter supports it; prime otherwise. This is
	// the default when HydrateOpts.Mode is empty.
	RenderModeAuto RenderMode = ""

	// RenderModeReplay emits the target CLI's native local format
	// (JSONL for claude). Full fidelity; same-CLI same-machine flows.
	RenderModeReplay RenderMode = "replay"

	// RenderModePrime emits a synthesized opening prompt summarizing
	// prior activity. Lossy by design; cross-CLI universal path.
	RenderModePrime RenderMode = "prime"
)

// HydrateOpts configures Hydrate.
type HydrateOpts struct {
	Mode RenderMode // empty / Auto = adapter chooses
	Cwd  string     // override session.Cwd; empty = use session's cwd as-captured
	// PlanContent is a pre-rendered "## Plan" body forwarded to the
	// prime-mode synthesizer (common.PrimeOpts.PlanContent). The caller
	// (typically `resleeve resume`) is responsible for resolving the
	// session's scope and calling memory.BuildContext; the adapter
	// stays free of memory-store coupling. Ignored in replay mode.
	// Empty → prime renders "(none captured)".
	PlanContent string
}

// HydrateResult reports what the adapter actually materialized.
type HydrateResult struct {
	Mode      RenderMode // what the adapter chose (never Auto)
	Path      string     // file path where state was materialized
	SessionID string     // may differ from input (prime mode mints a new id)
	Notes     []string   // human-readable warnings ("3 tool_calls flattened to text")
}
