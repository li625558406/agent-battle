package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if err := judge.SignDir(dir, []byte(DevJudgeKey)); err != nil {
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
		JudgeKey: []byte(DevJudgeKey),
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
		JudgeKey: []byte(DevJudgeKey),
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
		JudgeKey: []byte(DevJudgeKey),
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

// slowAdapter 模拟不遵守完成语义的 agent：持续产出事件，
// 只有 ctx 取消后才返回（复现 Launch 因孤儿子进程持有 stdout 管道而挂死的场景）。
type slowAdapter struct{}

func (slowAdapter) Name() string  { return "slow" }
func (slowAdapter) Detect() error { return nil }

func (slowAdapter) Launch(ctx context.Context, cwd, taskDescription string,
	env []string, out chan<- adapter.RawEvent) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- adapter.RawEvent{Type: protocol.EventMessage, Note: "still-running"}:
		}
	}
}

// TestSessionRunTimeout 验证超时路径：Run 必须及时返回明确错误，
// 而不是随 Launch 永久阻塞（防回归：一旦 agentLoop 不监听 runCtx 即挂死）。
func TestSessionRunTimeout(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  slowAdapter{},
		TaskDir:  taskDir,
		Label:    "slow",
		JudgeKey: []byte(DevJudgeKey),
		Timeout:  50 * time.Millisecond,
	}
	start := time.Now()
	_, err := Run(context.Background(), cfg)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("超时后 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息应包含超时: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run 未及时返回（耗时 %v），疑似永久挂死", elapsed)
	}
}

// TestSessionRunInvalidConfig 验证非法配置返回明确错误而非 panic。
func TestSessionRunInvalidConfig(t *testing.T) {
	if _, err := Run(context.Background(), Config{TaskDir: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "Adapter 未设置") {
		t.Fatalf("Adapter 为 nil 应返回明确错误, got: %v", err)
	}
	taskDir := newTask(t)
	cfg := Config{Adapter: slowAdapter{}, TaskDir: taskDir, Timeout: -1}
	if _, err := Run(context.Background(), cfg); err == nil ||
		!strings.Contains(err.Error(), "不能为负") {
		t.Fatalf("Timeout 为负应返回明确错误, got: %v", err)
	}
}
