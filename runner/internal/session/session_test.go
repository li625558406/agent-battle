package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/judge"
)

// newTask 在临时目录构造一个 echo 可解的任务。
// 注意：judge.Run 会验签 tests/manifest.json，因此这里写完 manifest 后
// 必须调用 judge.SignDir 生成 tests/sig（HMAC-SHA256, key 与 judge 测试一致）。
func newTask(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := protocol.TaskManifest{TaskID: "t1", Name: "fix-add",
		Description: "fix calc.sh", TimeoutSec: 30}
	writeJSON(t, filepath.Join(dir, "task.json"), task)
	if err := os.WriteFile(filepath.Join(dir, "seed", "calc.sh"),
		[]byte("add() { echo $(( $1 - $2 )); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := protocol.JudgeManifest{TaskID: "t1", Tests: []protocol.TestCommand{
		{Name: "add-works", Cmd: `bash -c 'source calc.sh; [ "$(add 2 3)" = "5" ]'`},
	}}
	writeJSON(t, filepath.Join(dir, "tests", "manifest.json"), m)
	if err := judge.SignDir(dir, []byte("dev-secret")); err != nil {
		t.Fatalf("sign manifest: %v", err)
	}
	return dir
}

func writeJSON(t *testing.T, p string, v interface{}) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRunPass(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  adapter.Echo{FixContent: "add() { echo $(( $1 + $2 )); }\n"},
		TaskDir:  taskDir,
		Label:    "A",
		JudgeKey: []byte("dev-secret"),
		Timeout:  30 * time.Second,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Passed != 1 || res.Report.Total != 1 {
		t.Fatalf("judge wrong: %+v", res.Report)
	}
	if len(res.Events) == 0 {
		t.Fatal("no events collected")
	}
	if err := protocol.Chain(res.Events); err != nil {
		t.Fatalf("event chain invalid: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "report.json")); err != nil {
		t.Fatalf("report.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "events.ndjson")); err != nil {
		t.Fatalf("events.ndjson missing: %v", err)
	}
}

func TestSessionRunAgentCrash(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  adapter.Echo{Fail: true},
		TaskDir:  taskDir,
		Label:    "crash",
		JudgeKey: []byte("dev-secret"),
		Timeout:  30 * time.Second,
	}
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("agent crash should surface as error")
	}
}

func TestSessionRunJudgeFail(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  adapter.Echo{}, // 不写修复 → 判分失败但不报错
		TaskDir:  taskDir,
		Label:    "fail",
		JudgeKey: []byte("dev-secret"),
		Timeout:  30 * time.Second,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Passed != 0 {
		t.Fatalf("expected 0 passed, got %d", res.Report.Passed)
	}
}
