// Package e2e 端到端回归：真实示例任务目录 → 沙箱 → agent → 事件链 →
// 判分 → 报告落盘，验证 runner 核心闭环整体可用。
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/session"
)

// findRepoRoot 从 cwd 向上逐层查找包含 go.mod 的目录（仓库根），
// 使测试不依赖工作目录。
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod，无法定位仓库根")
		}
		dir = parent
	}
}

// TestFullLoopEcho 验证核心闭环：examples/fix-add 任务下，携带正确修复的
// Echo（A）对阵不修复的 Echo（B），跑 2 局。A 的修复用位置参数
// $1 $2（与 seed/calc.sh 签名一致）；B 不写文件（Echo 语义：FixContent
// 为空则不落盘），保留 seed 的 a-b bug。每局判分 A 2/2、B 1/2（B 的
// add-basic 失败：add(2,3)=-1、add(10,-4)=14 均不符；1 分来自
// file-only-change 零改动路径），A 按通过比例必胜，WinsA==2
// 确定性成立。
func TestFullLoopEcho(t *testing.T) {
	taskDir := filepath.Join(findRepoRoot(t), "examples", "fix-add")
	if _, err := os.Stat(filepath.Join(taskDir, "task.json")); err != nil {
		t.Skipf("示例任务缺失(%s)，跳过: %v", taskDir, err)
	}
	outDir := t.TempDir()
	cfg := session.MirrorConfig{
		TaskDir:  taskDir,
		JudgeKey: []byte(session.DevJudgeKey),
		Rounds:   2,
		OutDir:   outDir,
		MakeA: func() adapter.Adapter {
			return adapter.Echo{FixContent: "add() { echo $(( $1 + $2 )); }\n"}
		},
		MakeB: func() adapter.Adapter { return adapter.Echo{} },
	}
	sum, err := session.Mirror(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.WinsA != 2 || sum.WinsB != 0 || sum.Ties != 0 {
		t.Fatalf("期望 A 2 局全胜, got: winsA=%d winsB=%d ties=%d errors=%d/%d",
			sum.WinsA, sum.WinsB, sum.Ties, sum.AErrors, sum.BErrors)
	}
	for _, d := range sum.Details {
		if d.PassA != 2 || d.TotalA != 2 {
			t.Fatalf("round %d: A 判分应为 2/2, got %d/%d", d.Round, d.PassA, d.TotalA)
		}
		if d.PassB != 1 || d.TotalB != 2 {
			t.Fatalf("round %d: B 判分应为 1/2, got %d/%d", d.Round, d.PassB, d.TotalB)
		}
	}

	// 取证报告落盘：A 第一局目录下 events.ndjson 逐行可反序列化，
	// 且整条事件 hash 链校验通过。
	dirs, err := filepath.Glob(filepath.Join(outDir, "*A-r1*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("A-r1 报告目录应恰好 1 个, got %d 个, err=%v", len(dirs), err)
	}
	f, err := os.Open(filepath.Join(dirs[0], "events.ndjson"))
	if err != nil {
		t.Fatalf("events.ndjson 不存在: %v", err)
	}
	defer f.Close()
	var evs []protocol.Event
	dec := json.NewDecoder(f)
	for dec.More() {
		var ev protocol.Event
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("事件行反序列化失败: %v", err)
		}
		evs = append(evs, ev)
	}
	if len(evs) == 0 {
		t.Fatal("事件流为空")
	}
	if err := protocol.Chain(evs); err != nil {
		t.Fatalf("事件 hash 链校验失败: %v", err)
	}
}
