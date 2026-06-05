package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mattkorwel/resleeve/internal/event"
)

// DefaultServerURL is opencode's default `opencode serve` address. The
// SSE stream lives at GET /event (verified
// server/routes/instance/httpapi/groups/event.ts:8).
const DefaultServerURL = "http://127.0.0.1:4096"

// sseEnvelope is the minimal shape of a GET /event frame. opencode's bus
// frames are `{type, properties}` objects (EventV2.define); we decode the
// type to dispatch and keep properties raw for the per-type handlers.
type sseEnvelope struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// fromSSEFrame decodes one GET /event frame (the JSON after `data:`) into
// events. Only the message/part frames carry conversation content we can
// normalize; control frames (server.connected, session.idle, …) yield no
// events. sessionHint supplies the session id when the frame's payload
// omits it.
//
// Note: the live SSE path is opt-in and best-effort — the db reader
// (ReconcileOnce) is the source of truth. Frames we don't recognize are
// dropped rather than erroring, so a schema drift on the bus never wedges
// capture.
func (a *Adapter) fromSSEFrame(raw []byte, sessionHint string) ([]event.Event, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var env sseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("opencode: decode sse frame: %w", err)
	}

	switch env.Type {
	case "session.created", "session.updated":
		return a.sseSessionStart(env.Properties)
	case "message.updated":
		return a.sseMessage(env.Properties, sessionHint)
	case "message.part.updated":
		return a.ssePart(env.Properties, sessionHint)
	default:
		// Control/streaming frames carry no committed conversation state we
		// normalize here; the db backfill captures the durable rows.
		return nil, nil
	}
}

// sseSessionStart maps a session.created/updated frame's `info` to a
// session_start event. The frame's info is a SessionInfo (directory on the
// session).
func (a *Adapter) sseSessionStart(props json.RawMessage) ([]event.Event, error) {
	var p struct {
		Info SessionInfo `json:"info"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return nil, fmt.Errorf("opencode: decode session frame: %w", err)
	}
	if p.Info.ID == "" {
		return nil, nil
	}
	row := sessionRow{
		ID:          p.Info.ID,
		ProjectID:   p.Info.ProjectID,
		Directory:   p.Info.Directory,
		Title:       p.Info.Title,
		Version:     p.Info.Version,
		TimeCreated: p.Info.Time.Created,
		TimeUpdated: p.Info.Time.Updated,
	}
	if p.Info.Model != nil {
		row.Model = mustJSON(p.Info.Model)
	}
	return []event.Event{sessionStartEvent(row, nil)}, nil
}

// sseMessage maps a message.updated frame's `info` (a SessionV1.Info) into
// its message-level events. Parts arrive in separate part frames, so the
// message frame alone only yields the concatenated text event once parts
// land; here we emit the message envelope with no parts (text is empty
// until parts arrive, so this is typically a no-op until the matching
// part.updated frames flow). It is kept for completeness / role + model.
func (a *Adapter) sseMessage(props json.RawMessage, sessionHint string) ([]event.Event, error) {
	var p struct {
		Info json.RawMessage `json:"info"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return nil, fmt.Errorf("opencode: decode message frame: %w", err)
	}
	id, sid := messageIDs(p.Info, sessionHint)
	if id == "" || sid == "" {
		return nil, nil
	}
	m := messageRow{ID: id, SessionID: sid, Data: p.Info}
	var md messageData
	_ = json.Unmarshal(p.Info, &md)
	m.TimeCreated = md.Time.Created
	// opencode message info has no version field of its own; the session
	// row carries the version, so pass "" here.
	return messageEvents(m, nil, "", ""), nil
}

// ssePart maps a message.part.updated frame's `part` (a SessionV1.Part)
// into its events, reusing the row mapper so SSE and db produce identical
// event_uuids for the same part.
func (a *Adapter) ssePart(props json.RawMessage, sessionHint string) ([]event.Event, error) {
	var p struct {
		Part json.RawMessage `json:"part"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return nil, fmt.Errorf("opencode: decode part frame: %w", err)
	}
	id, sid, mid := partIDs(p.Part, sessionHint)
	if id == "" || sid == "" {
		return nil, nil
	}
	pr := partRow{ID: id, MessageID: mid, SessionID: sid, Data: p.Part}
	mrow := messageRow{ID: mid, SessionID: sid}
	return messageEvents(mrow, []partRow{pr}, "", ""), nil
}

// messageIDs extracts (id, sessionID) from a raw SessionV1.Info blob.
func messageIDs(raw json.RawMessage, sessionHint string) (id, sessionID string) {
	var ids struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
	}
	_ = json.Unmarshal(raw, &ids)
	sid := ids.SessionID
	if sid == "" {
		sid = sessionHint
	}
	return ids.ID, sid
}

// partIDs extracts (id, sessionID, messageID) from a raw SessionV1.Part.
func partIDs(raw json.RawMessage, sessionHint string) (id, sessionID, messageID string) {
	var ids struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
	}
	_ = json.Unmarshal(raw, &ids)
	sid := ids.SessionID
	if sid == "" {
		sid = sessionHint
	}
	return ids.ID, sid, ids.MessageID
}

// Subscribe opens GET <serverURL>/event and feeds each decoded frame to
// ing.IngestBatch until ctx is cancelled or the stream ends. It is the
// optional live channel; the db reader remains the source of truth.
//
// Auth: when $OPENCODE_SERVER_PASSWORD is set it is sent as a Bearer
// token (best-effort; resolve the exact scheme against /doc on a live
// install — recorded as an open question).
func (a *Adapter) Subscribe(ctx context.Context, serverURL string, password string, ing Ingester) error {
	if strings.TrimSpace(serverURL) == "" {
		serverURL = DefaultServerURL
	}
	url := strings.TrimRight(serverURL, "/") + "/event"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("opencode: build sse request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if password != "" {
		req.Header.Set("Authorization", "Bearer "+password)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: open sse stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode: sse stream returned %s", resp.Status)
	}

	return a.consumeSSE(ctx, resp.Body, ing)
}

// consumeSSE parses a text/event-stream body, reassembling multi-line
// `data:` payloads per the SSE spec (frames separated by a blank line).
func (a *Adapter) consumeSSE(ctx context.Context, body io.Reader, ing Ingester) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var data strings.Builder
	flush := func() {
		defer data.Reset()
		payload := strings.TrimSpace(data.String())
		if payload == "" {
			return
		}
		events, err := a.fromSSEFrame([]byte(payload), "")
		if err != nil || len(events) == 0 {
			return
		}
		sid := events[0].SessionID
		_ = ing.IngestBatch(ctx, sid, events)
	}

	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(line, "data:"))
		default:
			// id:/event:/retry:/comment lines — ignored.
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("opencode: read sse stream: %w", err)
	}
	return nil
}
