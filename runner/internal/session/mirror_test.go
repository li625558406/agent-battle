package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbattle/runner/internal/adapter"
)

// TestMirrorEchoAFixedBNot 验证基础胜负：A 修复、B 不修复 → A 全胜。
func TestMirrorEchoAFixedBNot(t *testing.T) {
	taskDir := newTask(t)
	outDir := t.TempDir()
	cfg := MirrorConfig{
		TaskDir:  taskDir,
		JudgeKey: []byte(DevJudgeKey),
		Rounds:   3,
		OutDir:   outDir,
		MakeA: func() adapter.Adapter {
			return adapter.Echo{FixContent: "add() { echo $(( $1 + $2 )); }\n", TargetFile: "calc.sh"}
		},
		MakeB: func() adapter.Adapter { return adapter.Echo{TargetFile: "calc.sh"} },
		EnvA:  []string{"CFG=A"},
		EnvB:  []string{"CFG=B"},
	}
	sum, err := Mirror(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.WinsA != 3 || sum.WinsB != 0 || sum.Ties != 0 {
		t.Fatalf("期望 A 全胜, got: winsA=%d winsB=%d ties=%d", sum.WinsA, sum.WinsB, sum.Ties)
	}
	if sum.AErrors != 0 || sum.BErrors != 0 {
		t.Fatalf("不应有错误: AErrors=%d BErrors=%d", sum.AErrors, sum.BErrors)
	}
	if len(sum.Details) != 3 {
		t.Fatalf("期望 3 条明细, got %d", len(sum.Details))
	}
	for _, d := range sum.Details {
		if d.Winner != "a" {
			t.Fatalf("round %d winner 应为 a, got %q", d.Round, d.Winner)
		}
		if d.PassA != 1 || d.TotalA != 1 || d.PassB != 0 || d.TotalB != 1 {
			t.Fatalf("round %d 判分明细不符: %+v", d.Round, d)
		}
		if d.DirA == "" || d.DirB == "" {
			t.Fatalf("round %d 报告目录缺失: %+v", d.Round, d)
		}
	}
	// 汇总应落盘到 OutDir
	matches, err := filepath.Glob(filepath.Join(outDir, "mirror-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("汇总文件应恰好 1 份: %v %v", matches, err)
	}
	if _, err := os.Stat(matches[0]); err != nil {
		t.Fatalf("汇总文件不可读: %v", err)
	}
}

// TestMirrorTieOnBothFail 验证双方都不修复时按平局计（双方 0 通过，
// 失败耗时没有竞速意义，不参与耗时决胜）。
func TestMirrorTieOnBothFail(t *testing.T) {
	taskDir := newTask(t)
	cfg := MirrorConfig{
		TaskDir:  taskDir,
		JudgeKey: []byte(DevJudgeKey),
		Rounds:   2,
		OutDir:   t.TempDir(),
		MakeA:    func() adapter.Adapter { return adapter.Echo{} },
		MakeB:    func() adapter.Adapter { return adapter.Echo{} },
	}
	sum, err := Mirror(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Ties != 2 || sum.WinsA != 0 || sum.WinsB != 0 {
		t.Fatalf("期望全平局, got: winsA=%d winsB=%d ties=%d", sum.WinsA, sum.WinsB, sum.Ties)
	}
}

// TestMirrorPreflightRejectsBadManifest：坏判分包（签名不符）必须在预检阶段
// 拦截并报错，而不是跑完 N 局后在判分阶段失败、污染整场统计。
func TestMirrorPreflightRejectsBadManifest(t *testing.T) {
	taskDir := newTask(t)
	// 破坏签名：改写 manifest 真实内容（追加空白不会改变 canonical 形态）。
	mPath := filepath.Join(taskDir, "tests", "manifest.json")
	b, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(b), `"t1"`, `"t9"`, 1)
	if tampered == string(b) {
		t.Fatal("tamper payload did not modify manifest")
	}
	if err := os.WriteFile(mPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := MirrorConfig{
		TaskDir: taskDir, JudgeKey: []byte(DevJudgeKey), Rounds: 1,
		OutDir: t.TempDir(),
		MakeA:  func() adapter.Adapter { return adapter.Echo{} },
		MakeB:  func() adapter.Adapter { return adapter.Echo{} },
	}
	if _, err := Mirror(context.Background(), cfg); err == nil {
		t.Fatal("bad manifest must fail preflight, not poison N rounds")
	}
}

// TestMirrorSummaryShape 验证 RoundsPlayed 语义：已分胜负 + 平局之和。
func TestMirrorSummaryShape(t *testing.T) {
	s := Summary{WinsA: 1, WinsB: 2, Ties: 3}
	if s.RoundsPlayed() != 6 {
		t.Fatalf("RoundsPlayed 应为 6, got %d", s.RoundsPlayed())
	}
}
