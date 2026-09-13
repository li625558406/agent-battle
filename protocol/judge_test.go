package protocol

import (
	"encoding/json"
	"testing"
)

func TestJudgeReportJSONShape(t *testing.T) {
	r := JudgeReport{TaskID: "fix-add", Passed: 1, Total: 2, DiffHash: "deadbeef",
		Results: []TestResult{{Name: "add-works", Passed: true, ExitCode: 0, LogHash: "aa"}}}
	b, _ := json.Marshal(r)
	want := `{"task_id":"fix-add","results":[{"name":"add-works","passed":true,"exit_code":0,"log_hash":"aa"}],"passed":1,"total":2,"diff_hash":"deadbeef"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestJudgeManifestParse(t *testing.T) {
	data := []byte(`{"task_id":"fix-add","tests":[{"name":"t1","cmd":"bash .judge/run_tests.sh"}]}`)
	var m JudgeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.TaskID != "fix-add" || len(m.Tests) != 1 || m.Tests[0].Cmd == "" {
		t.Fatalf("bad parse: %+v", m)
	}
}
