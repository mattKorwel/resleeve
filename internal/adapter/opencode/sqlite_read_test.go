package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// newFixtureDB creates an opencode.db fixture with the SessionTable /
// MessageTable / PartTable subset resleeve reads, populated from the given
// rows. Returns the db path. The schema mirrors session/sql.ts columns we
// SELECT (only those; the real table has more, which is fine).
func newFixtureDB(t *testing.T, sessions []sessionRow, messages []messageRow, parts []partRow) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			directory TEXT,
			title TEXT,
			version TEXT,
			model TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL
		)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	for _, s := range sessions {
		if _, err := db.Exec(
			`INSERT INTO session (id, project_id, directory, title, version, model, time_created, time_updated) VALUES (?,?,?,?,?,?,?,?)`,
			s.ID, s.ProjectID, s.Directory, s.Title, s.Version, nullableJSON(s.Model), s.TimeCreated, s.TimeUpdated,
		); err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}
	for _, m := range messages {
		if _, err := db.Exec(
			`INSERT INTO message (id, session_id, time_created, data) VALUES (?,?,?,?)`,
			m.ID, m.SessionID, m.TimeCreated, string(m.Data),
		); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	for _, p := range parts {
		if _, err := db.Exec(
			`INSERT INTO part (id, message_id, session_id, time_created, data) VALUES (?,?,?,?,?)`,
			p.ID, p.MessageID, p.SessionID, p.TimeCreated, string(p.Data),
		); err != nil {
			t.Fatalf("insert part: %v", err)
		}
	}
	return dbPath
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func TestReadSessionEvents_OrdersAndMaps(t *testing.T) {
	sessions := []sessionRow{{
		ID:          "ses_001",
		ProjectID:   "prj_1",
		Directory:   "/work/myproj",
		Title:       "hello",
		Version:     "0.4.0",
		Model:       json.RawMessage(`{"id":"claude","providerID":"anthropic"}`),
		TimeCreated: 1000,
		TimeUpdated: 1000,
	}}
	// Data blobs match the REAL on-disk layout: opencode hoists
	// id/sessionID (messages) and id/sessionID/messageID (parts) into their
	// own columns, so they are NOT present inside the data blob. The reader
	// must re-attach the column ids when building native payloads.
	messages := []messageRow{
		{ID: "msg_001", SessionID: "ses_001", TimeCreated: 1100, Data: json.RawMessage(`{"role":"user","time":{"created":1100}}`)},
		{ID: "msg_002", SessionID: "ses_001", TimeCreated: 1200, Data: json.RawMessage(`{"role":"assistant","modelID":"claude","providerID":"anthropic","time":{"created":1200}}`)},
	}
	parts := []partRow{
		{ID: "prt_001", MessageID: "msg_001", SessionID: "ses_001", TimeCreated: 1100, Data: json.RawMessage(`{"type":"text","text":"port this"}`)},
		{ID: "prt_002", MessageID: "msg_002", SessionID: "ses_001", TimeCreated: 1200, Data: json.RawMessage(`{"type":"text","text":"on it"}`)},
		{ID: "prt_003", MessageID: "msg_002", SessionID: "ses_001", TimeCreated: 1201, Data: json.RawMessage(`{"type":"tool","callID":"call_1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"a\nb"}}`)},
	}

	dbPath := newFixtureDB(t, sessions, messages, parts)
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()

	events, err := readSessionEvents(context.Background(), db)
	if err != nil {
		t.Fatalf("readSessionEvents: %v", err)
	}

	// Expect: session_start, user_message, assistant_message, tool_call, tool_result.
	kinds := map[event.Kind]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds[event.KindSessionStart] != 1 {
		t.Errorf("session_start count: got %d, want 1", kinds[event.KindSessionStart])
	}
	if kinds[event.KindUserMessage] != 1 {
		t.Errorf("user_message count: got %d, want 1", kinds[event.KindUserMessage])
	}
	if kinds[event.KindAssistantMessage] != 1 {
		t.Errorf("assistant_message count: got %d, want 1", kinds[event.KindAssistantMessage])
	}
	if kinds[event.KindToolCall] != 1 || kinds[event.KindToolResult] != 1 {
		t.Errorf("tool call/result counts: got call=%d result=%d, want 1/1", kinds[event.KindToolCall], kinds[event.KindToolResult])
	}

	// First event must be session_start, with scope derived from directory.
	if events[0].Kind != event.KindSessionStart {
		t.Fatalf("first event kind: got %q, want session_start", events[0].Kind)
	}
	if events[0].Slot.Scope != "myproj" {
		t.Errorf("session_start scope: got %q, want myproj (from directory)", events[0].Slot.Scope)
	}

	// Ordering: seq strictly non-decreasing.
	for i := 1; i < len(events); i++ {
		if events[i].Seq < events[i-1].Seq {
			t.Errorf("events not seq-ordered at %d: %d < %d", i, events[i].Seq, events[i-1].Seq)
		}
	}

	// Tool call/result must carry the tool input/output paired by callID.
	var call, res event.Event
	for _, e := range events {
		switch e.Kind {
		case event.KindToolCall:
			call = e
		case event.KindToolResult:
			res = e
		}
	}
	var callC struct {
		Tool struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"tool"`
	}
	if err := json.Unmarshal(call.Content, &callC); err != nil {
		t.Fatalf("unmarshal call content: %v", err)
	}
	if callC.Tool.ID != "call_1" || callC.Tool.Name != "bash" {
		t.Errorf("tool call id/name: got %q/%q, want call_1/bash", callC.Tool.ID, callC.Tool.Name)
	}
	if string(callC.Tool.Args) == "" || string(callC.Tool.Args) == "null" {
		t.Errorf("tool call args missing input: %s", callC.Tool.Args)
	}
	var resC struct {
		Tool struct {
			CallID string `json:"call_id"`
			Status string `json:"status"`
			Result string `json:"result"`
		} `json:"tool"`
	}
	if err := json.Unmarshal(res.Content, &resC); err != nil {
		t.Fatalf("unmarshal result content: %v", err)
	}
	if resC.Tool.CallID != "call_1" || resC.Tool.Status != "success" {
		t.Errorf("tool result call_id/status: got %q/%q, want call_1/success", resC.Tool.CallID, resC.Tool.Status)
	}
	if resC.Tool.Result != "a\nb" {
		t.Errorf("tool result body: got %q, want %q", resC.Tool.Result, "a\nb")
	}
}

func TestReadSessionEvents_ConcatenatesText(t *testing.T) {
	sessions := []sessionRow{{ID: "ses_x", ProjectID: "p", Directory: "/d", Title: "t", Version: "0.4.0", TimeCreated: 1, TimeUpdated: 1}}
	messages := []messageRow{{ID: "msg_x", SessionID: "ses_x", TimeCreated: 2, Data: json.RawMessage(`{"role":"assistant","time":{"created":2}}`)}}
	parts := []partRow{
		{ID: "prt_a", MessageID: "msg_x", SessionID: "ses_x", TimeCreated: 2, Data: json.RawMessage(`{"type":"text","text":"foo "}`)},
		{ID: "prt_b", MessageID: "msg_x", SessionID: "ses_x", TimeCreated: 3, Data: json.RawMessage(`{"type":"text","text":"bar"}`)},
	}
	dbPath := newFixtureDB(t, sessions, messages, parts)
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	events, err := readSessionEvents(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range events {
		if e.Kind == event.KindAssistantMessage {
			var c struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.Content, &c)
			got = c.Text
		}
	}
	if got != "foo bar" {
		t.Errorf("concatenated text: got %q, want %q", got, "foo bar")
	}
}

// TestReadThenReplay_RoundTripsMessageText is the critical regression for
// the replay content blocker: capture a user + assistant turn (each with a
// real-layout, hoisted-id text part) from the db, render the import JSON via
// ToNative, and assert the message TEXT survives as a complete, decodable
// SessionV1.Part inside the message's parts[]. Before the fix, conversational
// text was dropped (parts:[]) and the re-attached ids were missing.
func TestReadThenReplay_RoundTripsMessageText(t *testing.T) {
	sessions := []sessionRow{{
		ID: "ses_rt", ProjectID: "p", Directory: "/work/rt", Title: "t",
		Version: "0.4.0", TimeCreated: 100, TimeUpdated: 100,
	}}
	// Real hoisted layout: ids in columns, not in the data blob.
	messages := []messageRow{
		{ID: "msg_u", SessionID: "ses_rt", TimeCreated: 110, Data: json.RawMessage(`{"role":"user","time":{"created":110}}`)},
		{ID: "msg_a", SessionID: "ses_rt", TimeCreated: 120, Data: json.RawMessage(`{"role":"assistant","modelID":"claude","providerID":"anthropic","time":{"created":120}}`)},
	}
	parts := []partRow{
		{ID: "prt_u1", MessageID: "msg_u", SessionID: "ses_rt", TimeCreated: 110, Data: json.RawMessage(`{"type":"text","text":"please port this"}`)},
		{ID: "prt_a1", MessageID: "msg_a", SessionID: "ses_rt", TimeCreated: 120, Data: json.RawMessage(`{"type":"reasoning","text":"thinking about it","time":{"start":1}}`)},
		{ID: "prt_a2", MessageID: "msg_a", SessionID: "ses_rt", TimeCreated: 121, Data: json.RawMessage(`{"type":"text","text":"on it now"}`)},
		{ID: "prt_a3", MessageID: "msg_a", SessionID: "ses_rt", TimeCreated: 122, Data: json.RawMessage(`{"type":"tool","callID":"c9","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"ok","time":{"start":1,"end":2}}}`)},
	}

	dbPath := newFixtureDB(t, sessions, messages, parts)
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()

	events, err := readSessionEvents(context.Background(), db)
	if err != nil {
		t.Fatalf("readSessionEvents: %v", err)
	}

	body, err := New().ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative replay: %v", err)
	}

	var file ImportFile
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatalf("import JSON did not parse: %v\n%s", err, body)
	}
	if len(file.Messages) != 2 {
		t.Fatalf("messages: got %d, want 2\n%s", len(file.Messages), body)
	}

	// collectText gathers every text/reasoning part's text plus the message
	// id/role from a reconstructed import message, asserting the hoisted ids
	// were re-attached (so the part validates against SessionV1.Part).
	type partView struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		Type      string `json:"type"`
		Text      string `json:"text"`
		CallID    string `json:"callID"`
	}
	byRole := map[string]ImportMessage{}
	for _, m := range file.Messages {
		var info struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Role      string `json:"role"`
		}
		if err := json.Unmarshal(m.Info, &info); err != nil {
			t.Fatalf("message info did not parse: %v", err)
		}
		if info.ID == "" || info.SessionID == "" {
			t.Errorf("message info missing re-attached ids: %s", m.Info)
		}
		byRole[info.Role] = m
	}

	// User turn: the text part must round-trip with all hoisted ids present.
	userMsg, ok := byRole["user"]
	if !ok {
		t.Fatalf("no user message in import JSON\n%s", body)
	}
	var userTexts []string
	for _, raw := range userMsg.Parts {
		var pv partView
		if err := json.Unmarshal(raw, &pv); err != nil {
			t.Fatalf("user part did not parse: %v", err)
		}
		if pv.Type == "text" {
			if pv.ID == "" || pv.SessionID == "" || pv.MessageID == "" {
				t.Errorf("text part missing hoisted ids: %s", raw)
			}
			if pv.MessageID != "msg_u" {
				t.Errorf("text part messageID: got %q, want msg_u", pv.MessageID)
			}
			userTexts = append(userTexts, pv.Text)
		}
	}
	if len(userTexts) != 1 || userTexts[0] != "please port this" {
		t.Errorf("user text did not round-trip: got %v, want [please port this]", userTexts)
	}

	// Assistant turn: reasoning + text + tool parts must all be present.
	asstMsg := byRole["assistant"]
	var sawReasoning, sawText, sawTool bool
	for _, raw := range asstMsg.Parts {
		var pv partView
		if err := json.Unmarshal(raw, &pv); err != nil {
			t.Fatalf("assistant part did not parse: %v", err)
		}
		switch pv.Type {
		case "reasoning":
			sawReasoning = pv.Text == "thinking about it"
		case "text":
			sawText = pv.Text == "on it now"
		case "tool":
			sawTool = pv.CallID == "c9"
		}
	}
	if !sawReasoning {
		t.Errorf("assistant reasoning part missing/wrong in import parts: %s", body)
	}
	if !sawText {
		t.Errorf("assistant text part missing/wrong in import parts: %s", body)
	}
	if !sawTool {
		t.Errorf("assistant tool part missing in import parts: %s", body)
	}
}
