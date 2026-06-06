package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/mattkorwel/resleeve/internal/event"
)

// buildUserPayload hand-crafts a step_type-14-shaped protobuf blob with the
// user prompt at proto path [19,2].
func buildUserPayload(prompt string) []byte {
	msg19 := encBytes(2, []byte(prompt))
	return encBytes(19, msg19)
}

// buildAssistantPayload puts assistant text at [20,1].
func buildAssistantPayload(text string) []byte {
	msg20 := encBytes(1, []byte(text))
	return encBytes(20, msg20)
}

// buildToolPayload puts tool name at [5,4,2] and JSON args at [5,4,3].
func buildToolPayload(name, args string) []byte {
	inner4 := append(encBytes(2, []byte(name)), encBytes(3, []byte(args))...)
	msg5 := encBytes(4, inner4)
	return encBytes(5, msg5)
}

func TestMapStep_User(t *testing.T) {
	a := New()
	s := step{Idx: 0, StepType: stUser, Payload: buildUserPayload("tell me about parallel agents")}
	evs := a.mapStep("conv-1", "myscope", s)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0]
	if e.Kind != event.KindUserMessage {
		t.Errorf("kind = %s", e.Kind)
	}
	if e.SessionID != "conv-1" || e.Slot.Scope != "myscope" || e.Seq != 0 {
		t.Errorf("base fields wrong: %+v", e)
	}
	var c map[string]any
	if err := json.Unmarshal(e.Content, &c); err != nil {
		t.Fatal(err)
	}
	if c["text"] != "tell me about parallel agents" {
		t.Errorf("text = %v", c["text"])
	}
	if len(e.Vendor.NativePayload) == 0 {
		t.Error("native payload not preserved")
	}
}

func TestMapStep_Assistant(t *testing.T) {
	a := New()
	s := step{Idx: 5, StepType: stAssistant, Payload: buildAssistantPayload("here is the answer")}
	evs := a.mapStep("conv-1", "s", s)
	if evs[0].Kind != event.KindAssistantMessage {
		t.Fatalf("kind = %s", evs[0].Kind)
	}
	var c map[string]any
	_ = json.Unmarshal(evs[0].Content, &c)
	if c["text"] != "here is the answer" {
		t.Errorf("text = %v", c["text"])
	}
}

func TestMapStep_ToolCall(t *testing.T) {
	a := New()
	args := `{"AbsolutePath":"/x/y","toolAction":"Viewing"}`
	s := step{Idx: 8, StepType: 8, Payload: buildToolPayload("view_file", args)}
	evs := a.mapStep("conv-1", "s", s)
	if evs[0].Kind != event.KindToolCall {
		t.Fatalf("kind = %s", evs[0].Kind)
	}
	var c struct {
		Tool struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"tool"`
	}
	if err := json.Unmarshal(evs[0].Content, &c); err != nil {
		t.Fatal(err)
	}
	if c.Tool.Name != "view_file" {
		t.Errorf("tool name = %q", c.Tool.Name)
	}
	if string(c.Tool.Args) != args {
		t.Errorf("tool args = %s", c.Tool.Args)
	}
}

func TestMapStep_UnknownTypeIsSystem(t *testing.T) {
	a := New()
	s := step{Idx: 3, StepType: 9999, Payload: encBytes(1, []byte("mystery payload text"))}
	evs := a.mapStep("conv-1", "s", s)
	if evs[0].Kind != event.KindSystem {
		t.Fatalf("kind = %s, want system", evs[0].Kind)
	}
	var c map[string]any
	_ = json.Unmarshal(evs[0].Content, &c)
	if c["resleeve.system.kind"] != "antigravity:unclassified" {
		t.Errorf("system kind = %v", c["resleeve.system.kind"])
	}
	if len(evs[0].Vendor.NativePayload) == 0 {
		t.Error("native payload must be preserved for unclassified steps")
	}
}

func TestMapStep_ContextAndTitleAreSystem(t *testing.T) {
	a := New()
	for _, st := range []int64{stContext, stTitle} {
		s := step{Idx: 1, StepType: st, Payload: encBytes(20, encBytes(1, []byte("some system text here")))}
		evs := a.mapStep("conv-1", "s", s)
		if evs[0].Kind != event.KindSystem {
			t.Errorf("step_type %d: kind = %s, want system", st, evs[0].Kind)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		st        int64
		kind      event.Kind
		confident bool
	}{
		{14, event.KindUserMessage, true},
		{15, event.KindAssistantMessage, true},
		{98, event.KindSystem, true},
		{23, event.KindSystem, true},
		{7, event.KindToolCall, true},
		{8, event.KindToolCall, true},
		{9, event.KindToolCall, true},
		{21, event.KindToolCall, true},
		{33, event.KindToolCall, true},
		{132, event.KindToolCall, true},
		{4242, event.KindSystem, false},
	}
	for _, c := range cases {
		k, conf := classify(c.st)
		if k != c.kind || conf != c.confident {
			t.Errorf("classify(%d) = (%s,%v), want (%s,%v)", c.st, k, conf, c.kind, c.confident)
		}
	}
}

func TestDeterministicEventUUID_Stable(t *testing.T) {
	a := DeterministicEventUUID("conv-x", stepKey(7))
	b := DeterministicEventUUID("conv-x", stepKey(7))
	if a != b {
		t.Fatalf("UUID not deterministic: %s != %s", a, b)
	}
	if c := DeterministicEventUUID("conv-y", stepKey(7)); c == a {
		t.Errorf("UUID collided across conversations")
	}
}
