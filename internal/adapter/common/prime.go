// Package common holds adapter-agnostic helpers shared by multiple CLI
// adapters. The first inhabitant is the prime-mode markdown synthesizer
// (see docs/design/round-4/01-conversation-transport.md): a renderer
// that turns a captured event stream into a single opening prompt
// summarizing prior activity. Lossy by design — the universal path
// when source CLI != target CLI (or when an adapter doesn't support
// replay).
package common

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// PrimeOpts tunes how SynthesizePrime renders.
type PrimeOpts struct {
	// RecentN caps how many recent activity rows are emitted. 0 → 20.
	RecentN int
	// MaxBytes caps the rendered output's size. 0 → ~16KB (roughly
	// 4000 tokens at ~4 bytes/token). Truncation happens by trimming
	// older "Recent activity" entries until under the cap; a note is
	// appended so the reader knows.
	MaxBytes int
	// PlanContent is optional pre-rendered "## Plan" body. Caller is
	// responsible for any memory-store lookup; the adapter doesn't
	// reach into the memory package on its own. Empty → "(none
	// captured)".
	PlanContent string
}

const (
	defaultRecentN  = 20
	defaultMaxBytes = 16 * 1024 // ≈ 4000 tokens
)

// SynthesizePrime renders a markdown opening prompt for a re-sleeved
// session. The output has four top-level sections:
//
//	# Resumed session: <session_id>
//	## Context        — source CLI, cwd, branch, model, start time
//	## Plan           — caller-provided memory rollup
//	## Recent activity — last N events, oldest first, ~1 line each
//	## Next           — last unanswered user prompt or open tool result
//
// Length cap: target ≤ opts.MaxBytes (default 16KB). If the recent
// activity section would push the document over the cap, oldest
// entries are dropped and a "(N earlier events omitted)" note is
// emitted. Thinking events are always dropped (internal, not for
// prompt context). Tool_call / tool_result pairs are matched by
// content.tool.call_id; calls with no result get "(no result
// captured)".
func SynthesizePrime(session adapter.SessionView, events []event.Event, opts PrimeOpts) ([]byte, error) {
	recentN := opts.RecentN
	if recentN <= 0 {
		recentN = defaultRecentN
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	sorted := make([]event.Event, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].EventUUID < sorted[j].EventUUID
	})

	// Index tool_results by call_id so we can pair them with calls.
	resultsByCall := make(map[string]toolResultSummary)
	for _, e := range sorted {
		if e.Kind != event.KindToolResult {
			continue
		}
		callID, summary := parseToolResult(e)
		if callID == "" {
			continue
		}
		resultsByCall[callID] = summary
	}

	// Build the prose for each event we want to render in Recent
	// activity (oldest first). Drop kinds the spec excludes from
	// prime: thinking, tool_result (folded into its call), session_*.
	type row struct {
		ts   time.Time
		line string
	}
	var rows []row
	for _, e := range sorted {
		switch e.Kind {
		case event.KindThinking, event.KindToolResult,
			event.KindSessionStart, event.KindSessionEnd:
			continue
		}
		line := renderRow(e, resultsByCall)
		if line == "" {
			continue
		}
		rows = append(rows, row{ts: e.Timestamp, line: line})
	}

	// Cap to recentN (keep most recent).
	omitted := 0
	if len(rows) > recentN {
		omitted = len(rows) - recentN
		rows = rows[len(rows)-recentN:]
	}

	// Pick "Next" — the last unanswered user_message, or the most
	// recent open tool_call (one with no matching result), preferring
	// user_message when both are present.
	nextLine := pickNext(sorted, resultsByCall)

	// Render.
	var b strings.Builder
	sid := session.SessionID
	if sid == "" {
		sid = "(unknown)"
	}
	fmt.Fprintf(&b, "# Resumed session: %s\n\n", sid)

	b.WriteString("## Context\n\n")
	fmt.Fprintf(&b, "- Source CLI: %s\n", joinCLIVersion(session.CLI, session.CLIVersion))
	fmt.Fprintf(&b, "- Working directory: %s\n", fallback(session.Cwd, "(unknown)"))
	fmt.Fprintf(&b, "- Git branch: %s\n", fallback(session.GitBranch, "(unknown)"))
	fmt.Fprintf(&b, "- Model: %s\n", fallback(session.Model, "(unknown)"))
	fmt.Fprintf(&b, "- Started: %s\n", fmtTime(session.StartedAt))
	b.WriteString("\n")

	b.WriteString("## Plan\n\n")
	plan := strings.TrimSpace(opts.PlanContent)
	if plan == "" {
		b.WriteString("(none captured)\n\n")
	} else {
		b.WriteString(plan)
		if !strings.HasSuffix(plan, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	// Render Recent activity into a sub-buffer so we can apply the
	// length cap by trimming oldest entries when over.
	prefix := b.String()
	const truncNote = "- ... (%d earlier events omitted)\n"

	// Compose a candidate tail and check size.
	composeTail := func(useRows []row, extraOmitted int) string {
		var t strings.Builder
		t.WriteString("## Recent activity\n\n")
		totalOmitted := omitted + extraOmitted
		if totalOmitted > 0 {
			fmt.Fprintf(&t, truncNote, totalOmitted)
		}
		if len(useRows) == 0 && totalOmitted == 0 {
			t.WriteString("(no recent activity)\n")
		}
		for _, r := range useRows {
			fmt.Fprintf(&t, "- %s %s\n", fmtTime(r.ts), r.line)
		}
		t.WriteString("\n## Next\n\n")
		if nextLine == "" {
			t.WriteString("(no open prompt or tool result; pick up wherever feels right)\n")
		} else {
			t.WriteString(nextLine)
			if !strings.HasSuffix(nextLine, "\n") {
				t.WriteByte('\n')
			}
		}
		return t.String()
	}

	useRows := rows
	extraOmitted := 0
	for {
		tail := composeTail(useRows, extraOmitted)
		if len(prefix)+len(tail) <= maxBytes {
			b.WriteString(tail)
			break
		}
		if len(useRows) == 0 {
			// Even the minimal tail blows the cap — emit it anyway;
			// callers asked for a too-small budget.
			b.WriteString(tail)
			break
		}
		// Drop the oldest entry and try again.
		useRows = useRows[1:]
		extraOmitted++
	}

	return []byte(b.String()), nil
}

// toolResultSummary is the one-line representation of a tool_result.
type toolResultSummary struct {
	status string
	body   string
}

func parseToolResult(e event.Event) (callID string, sum toolResultSummary) {
	var payload struct {
		Tool struct {
			CallID string          `json:"call_id"`
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		} `json:"tool"`
	}
	if err := json.Unmarshal(e.Content, &payload); err != nil {
		return "", toolResultSummary{}
	}
	return payload.Tool.CallID, toolResultSummary{
		status: payload.Tool.Status,
		body:   summarizeJSON(payload.Tool.Result, 120),
	}
}

func renderRow(e event.Event, resultsByCall map[string]toolResultSummary) string {
	switch e.Kind {
	case event.KindUserMessage:
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Content, &c)
		return "user: " + truncate(oneLine(c.Text), 200)
	case event.KindAssistantMessage:
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Content, &c)
		return "assistant: " + truncate(oneLine(c.Text), 200)
	case event.KindToolCall:
		var c struct {
			Tool struct {
				ID   string          `json:"id"`
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"tool"`
		}
		_ = json.Unmarshal(e.Content, &c)
		args := summarizeJSON(c.Tool.Args, 80)
		res, ok := resultsByCall[c.Tool.ID]
		var resPart string
		if !ok {
			resPart = "(no result captured)"
		} else if res.status == "error" {
			resPart = "error: " + res.body
		} else {
			resPart = res.body
			if resPart == "" {
				resPart = "(empty result)"
			}
		}
		return fmt.Sprintf("called %s(%s) → %s", fallback(c.Tool.Name, "?"), args, resPart)
	case event.KindAttachment:
		// Look for a ref-ish field in vendor.system kind name; fall
		// back to a generic note.
		var c map[string]any
		_ = json.Unmarshal(e.Content, &c)
		ref := "(attachment)"
		if v, ok := c["ref"].(string); ok && v != "" {
			ref = v
		} else if v, ok := c["path"].(string); ok && v != "" {
			ref = v
		}
		return "(attachment: " + ref + ")"
	case event.KindSystem, event.KindError:
		// Skip noisy system rows; they rarely add prompt context.
		return ""
	}
	return ""
}

// pickNext picks the "Next" framing — last unanswered user_message,
// or the most recent open tool_call. Returns "" if neither.
func pickNext(sorted []event.Event, resultsByCall map[string]toolResultSummary) string {
	// Walk backwards for the most-recent user_message.
	for i := len(sorted) - 1; i >= 0; i-- {
		e := sorted[i]
		if e.Kind == event.KindUserMessage {
			var c struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.Content, &c)
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			return "Last user message — here's where we left off:\n\n> " +
				indentQuote(text)
		}
	}
	// Walk backwards for the most recent unpaired tool_call.
	for i := len(sorted) - 1; i >= 0; i-- {
		e := sorted[i]
		if e.Kind != event.KindToolCall {
			continue
		}
		var c struct {
			Tool struct {
				ID   string          `json:"id"`
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"tool"`
		}
		_ = json.Unmarshal(e.Content, &c)
		if _, ok := resultsByCall[c.Tool.ID]; ok {
			continue
		}
		return fmt.Sprintf("Open tool call awaiting result: %s(%s)",
			fallback(c.Tool.Name, "?"), summarizeJSON(c.Tool.Args, 200))
	}
	return ""
}

// --- formatting helpers ---

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "(unknown)"
	}
	return t.UTC().Format(time.RFC3339)
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func joinCLIVersion(name, version string) string {
	switch {
	case name == "" && version == "":
		return "(unknown)"
	case version == "":
		return name
	case name == "":
		return version
	}
	return name + " " + version
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// summarizeJSON renders a JSON value into a compact one-line summary,
// capping length. Strings render as their value (no surrounding quotes
// stripped); objects/arrays render via their JSON encoding.
func summarizeJSON(raw json.RawMessage, max int) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	// Compact whitespace.
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if compact, err := json.Marshal(v); err == nil {
			s = string(compact)
		}
	}
	s = oneLine(s)
	return truncate(s, max)
}

func indentQuote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	// First line already prefixed by caller's "> "; trim our prefix
	// off line 0 to avoid double-quoting.
	if strings.HasPrefix(lines[0], "> ") {
		lines[0] = strings.TrimPrefix(lines[0], "> ")
	}
	return strings.Join(lines, "\n")
}
