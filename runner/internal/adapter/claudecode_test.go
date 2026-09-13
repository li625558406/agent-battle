package adapter

import (
	"strings"
	"testing"
)

func TestParseLineToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"x.py","old_string":"a","new_string":"b"}}]}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events (tool_call + file_edit), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != "tool_call" || evs[0].Tool != "Edit" {
		t.Fatalf("bad ev0: %+v", evs[0])
	}
	if evs[0].ArgsHash == "" || strings.Contains(evs[0].ArgsHash, "old_string") {
		t.Fatal("ArgsHash must be a hash, never raw args")
	}
	if evs[1].Type != "file_edit" || evs[1].Note != "x.py" {
		t.Fatalf("bad ev1: %+v", evs[1])
	}
}

func TestParseLineResult(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":4200,"num_turns":3,"usage":{"input_tokens":100,"output_tokens":50}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "result" || evs[0].DurationMS != 4200 || evs[0].Tokens != 150 {
		t.Fatalf("bad: %+v err=%v", evs, err)
	}
}

func TestParseLineIgnorable(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`not json at all`,
		``,
	} {
		evs, err := parseLine([]byte(line))
		if err != nil {
			t.Fatalf("line %q: unexpected error %v", line, err)
		}
		if len(evs) > 1 {
			t.Fatalf("line %q: too many events %v", line, evs)
		}
	}
}

func TestParseLineMultipartContent(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tool != "Bash" {
		t.Fatalf("bad: %+v", evs)
	}
}

func TestParseLineResultIsError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"duration_ms":100,"usage":{"input_tokens":1,"output_tokens":1}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "error" {
		t.Fatalf("bad: %+v", evs)
	}
}
