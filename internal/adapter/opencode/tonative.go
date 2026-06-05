package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
	"github.com/mattkorwel/resleeve/internal/event"
)

// ToNative renders a captured event stream into opencode's native input.
//
// Replay mode (opencode → opencode): emit the `opencode import` transcript
// JSON ({info, messages:[{info, parts}]}). The reconstruction prefers each
// event's vendor.native_payload (the original SessionV1 row JSON captured
// from opencode.db) so the shape validates against SessionV1.Info /
// SessionV1.Part byte-for-byte. Events lacking a native payload (e.g. a
// synthesized session_start) are folded in from their normalized content
// where possible; messages with neither are skipped.
//
// Prime mode (cross-CLI, or replay fallback): synthesize a markdown
// opening prompt via common.SynthesizePrime. Lossy by design.
//
// Auto picks replay (opencode can natively ingest its own transcript).
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, mode adapter.RenderMode) ([]byte, error) {
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	switch mode {
	case adapter.RenderModeReplay:
		return buildImportJSON(events)
	case adapter.RenderModePrime:
		return common.SynthesizePrime(sessionViewFromEvents(events), events, common.PrimeOpts{})
	default:
		return nil, fmt.Errorf("opencode.ToNative: unknown mode %q", mode)
	}
}

// buildImportJSON reconstructs the import transcript from events. The
// session info comes from the session_start event (preferring its native
// payload); messages are reconstructed from each message/part event's
// native payload, grouped by message id.
func buildImportJSON(events []event.Event) ([]byte, error) {
	sorted := make([]event.Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].EventUUID < sorted[j].EventUUID
	})

	file := ImportFile{}

	// messages preserves first-seen order; msgIndex maps a message id to
	// its slot in file.Messages so parts attach to the right message.
	msgIndex := map[string]int{}
	ensureMessage := func(msgID string, info json.RawMessage) int {
		if idx, ok := msgIndex[msgID]; ok {
			if len(file.Messages[idx].Info) == 0 && len(info) > 0 {
				file.Messages[idx].Info = info
			}
			return idx
		}
		file.Messages = append(file.Messages, ImportMessage{Info: info})
		idx := len(file.Messages) - 1
		msgIndex[msgID] = idx
		return idx
	}

	for _, e := range sorted {
		switch e.Kind {
		case event.KindSessionStart:
			if len(file.Info) == 0 {
				file.Info = sessionInfoFromEvent(e)
			}
		case event.KindUserMessage, event.KindAssistantMessage:
			// Message-level events carry a {info, parts} envelope: info is
			// the full SessionV1.Info; parts are the message's text/reasoning
			// Part[] (tool parts arrive on their own KindToolCall events).
			info, parts := unwrapMessagePayload(e.Vendor.NativePayload)
			msgID := messageInfoID(info)
			if msgID == "" {
				continue
			}
			idx := ensureMessage(msgID, info)
			for _, p := range parts {
				if len(p) > 0 {
					file.Messages[idx].Parts = append(file.Messages[idx].Parts, p)
				}
			}
		case event.KindThinking:
			// Reasoning is carried both here and in its message envelope's
			// parts; the envelope is the import carrier, so skip to avoid a
			// duplicate part.
			continue
		case event.KindToolCall:
			// Tool parts carry the full SessionV1.Part; attach to the owning
			// message. The message info comes from the message event.
			msgID := messageIDForEvent(e)
			if msgID == "" {
				continue
			}
			idx := ensureMessage(msgID, nil)
			if len(e.Vendor.NativePayload) > 0 {
				file.Messages[idx].Parts = append(file.Messages[idx].Parts, e.Vendor.NativePayload)
			}
		case event.KindToolResult:
			// Result is folded into the tool part's native payload (same
			// payload as the call); skip to avoid duplicate parts.
			continue
		}
	}

	// Drop messages we couldn't reconstruct any info for — import.ts
	// requires a decodable SessionV1.Info per message.
	pruned := file.Messages[:0]
	for _, m := range file.Messages {
		if len(m.Info) == 0 {
			continue
		}
		pruned = append(pruned, m)
	}
	file.Messages = pruned

	if len(file.Info) == 0 {
		// No session_start payload: synthesize a minimal-but-valid info so
		// import has something to anchor on. import.ts overrides
		// directory/path/projectID itself, so this is acceptable.
		file.Info = synthSessionInfo(sorted)
	}

	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("opencode.ToNative: marshal import json: %w", err)
	}
	return out, nil
}

// sessionInfoFromEvent returns the session info for a session_start event,
// preferring its native payload (the verbatim SessionTable-derived
// SessionV1 info). Falls back to a constructed SessionInfo from content.
func sessionInfoFromEvent(e event.Event) json.RawMessage {
	if len(e.Vendor.NativePayload) > 0 {
		return e.Vendor.NativePayload
	}
	var c struct {
		Cwd   string `json:"cwd"`
		Model string `json:"model"`
		Title string `json:"title"`
	}
	_ = json.Unmarshal(e.Content, &c)
	info := SessionInfo{
		ID:        e.SessionID,
		Slug:      e.SessionID,
		Directory: c.Cwd,
		Title:     fallbackStr(c.Title, "resleeved session"),
		Version:   e.Vendor.Version,
		Time: SessionInfoTime{
			Created: e.Timestamp.UnixMilli(),
			Updated: e.Timestamp.UnixMilli(),
		},
	}
	return mustJSON(info)
}

// synthSessionInfo builds a minimal session info when no session_start was
// captured, sourcing the id from the first event that has one.
func synthSessionInfo(sorted []event.Event) json.RawMessage {
	var sid string
	var version string
	for _, e := range sorted {
		if e.SessionID != "" {
			sid = e.SessionID
			version = e.Vendor.Version
			break
		}
	}
	info := SessionInfo{
		ID:      sid,
		Slug:    sid,
		Title:   "resleeved session",
		Version: version,
	}
	return mustJSON(info)
}

// messageIDForEvent extracts the message id a tool-part event belongs to.
// Tool parts carry the full SessionV1.Part, so the owning message id is the
// payload's messageID. (Message-level events use unwrapMessagePayload /
// messageInfoID instead.)
func messageIDForEvent(e event.Event) string {
	if len(e.Vendor.NativePayload) == 0 {
		return ""
	}
	var ids struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
	}
	_ = json.Unmarshal(e.Vendor.NativePayload, &ids)
	if ids.MessageID != "" {
		return ids.MessageID // a part payload
	}
	return ids.ID // a bare message payload (fallback)
}

// unwrapMessagePayload splits a message event's native payload into its
// SessionV1.Info and its text/reasoning Part[]. The db reader / SSE wrap a
// message as {info, parts}; for resilience a bare info object (no wrapper)
// is treated as info with no parts.
func unwrapMessagePayload(payload json.RawMessage) (info json.RawMessage, parts []json.RawMessage) {
	if len(payload) == 0 {
		return nil, nil
	}
	var env struct {
		Info  json.RawMessage   `json:"info"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(payload, &env); err == nil && len(env.Info) > 0 {
		return env.Info, env.Parts
	}
	// Not a wrapper — treat the whole payload as the message info.
	return payload, nil
}

// messageInfoID reads the message id from a SessionV1.Info blob.
func messageInfoID(info json.RawMessage) string {
	if len(info) == 0 {
		return ""
	}
	var ids struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(info, &ids)
	return ids.ID
}

// sessionViewFromEvents constructs a minimal SessionView from the events
// themselves, for ToNative-direct callers without one.
func sessionViewFromEvents(events []event.Event) adapter.SessionView {
	var view adapter.SessionView
	for _, e := range events {
		if e.SessionID != "" {
			view.SessionID = e.SessionID
		}
		if e.Kind == event.KindSessionStart {
			var c struct {
				Cwd   string `json:"cwd"`
				Model string `json:"model"`
			}
			_ = json.Unmarshal(e.Content, &c)
			if view.Cwd == "" {
				view.Cwd = c.Cwd
			}
			if view.Model == "" {
				view.Model = c.Model
			}
		}
		if view.CLIVersion == "" {
			view.CLIVersion = e.Vendor.Version
		}
	}
	view.CLI = Name
	return view
}

// importSessionID extracts the session id from a rendered import JSON blob
// (info.id). Used by Hydrate to thread opencode's preserved session id
// into NativeResumeCmd. opencode import preserves the provided id
// (verified import.ts: inserts exportData.info.id).
func importSessionID(body []byte) string {
	var f struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return ""
	}
	return strings.TrimSpace(f.Info.ID)
}
