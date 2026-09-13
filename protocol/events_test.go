package protocol

import (
	"encoding/json"
	"testing"
)

func TestEventJSONShape(t *testing.T) {
	e := Event{Seq: 1, TS: 1700000000000, Type: EventToolCall, Tool: "Bash",
		ArgsHash: "abc", DurationMS: 12, Tokens: 0, Note: "/tmp/x.py"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"seq":1,"ts":1700000000000,"type":"tool_call","tool":"Bash","args_hash":"abc","duration_ms":12,"note":"/tmp/x.py","hash":""}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestEventTypeConstants(t *testing.T) {
	for _, s := range []string{EventToolCall, EventFileEdit, EventMessage, EventError, EventResult} {
		if s == "" {
			t.Fatal("empty event type constant")
		}
	}
}
