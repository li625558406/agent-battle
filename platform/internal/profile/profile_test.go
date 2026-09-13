// profile_test.go —— 归一化与聚合的边界测试。
package profile

import (
	"testing"
)

func nearly(a, b float64) bool {
	const eps = 1e-9
	return (a-b) < eps && (b-a) < eps
}

// TestPercentileRank 基线百分位的并列中位语义。
func TestPercentileRank(t *testing.T) {
	vals := []float64{0.5, 0.5, 1, 1}
	if got := pct(vals, 1); !nearly(got, 75) { // less=2, eq=2 → (2+1)/4
		t.Fatalf("pct(1) 应 75, got %v", got)
	}
	if got := pct(vals, 0.5); !nearly(got, 25) {
		t.Fatalf("pct(0.5) 应 25, got %v", got)
	}
	if got := pct(vals, 0); got != 0 {
		t.Fatalf("pct(0) 应 0, got %v", got)
	}
	if got := pct(nil, 1); !nearly(got, 50) {
		t.Fatalf("空基线应取中位 50, got %v", got)
	}
}

// TestBuildProfileKnownDifference 验收口径的最小复现：A 全通过 vs B 半通过，
// 各 2 局（基线 = 4 局全体池）→ A 正确性 > B 正确性，且 A 高于 B 的幅度
// 与百分位公式一致。
func TestBuildProfileKnownDifference(t *testing.T) {
	good := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 4, Tokens: 100, WallMS: 100}
	bad := MatchMetrics{PassRatio: 0.5, HasEvents: true, ToolCalls: 8, Tokens: 200, WallMS: 300}
	ownA := []MatchMetrics{good, good}
	ownB := []MatchMetrics{bad, bad}
	baseline := []MatchMetrics{good, good, bad, bad}

	pa := BuildProfile("A", "debug", ownA, baseline, 1000)
	pb := BuildProfile("B", "debug", ownB, baseline, 1000)

	if pa.SampleSize != 2 || !pa.LowSample { // 2 < 5
		t.Fatalf("样本量与 low_sample 标注错误: %+v", pa)
	}
	if pa.Dims["correctness"].Score <= pb.Dims["correctness"].Score {
		t.Fatalf("已知差异未复现: A=%v B=%v", pa.Dims["correctness"], pb.Dims["correctness"])
	}
	// 精确值：A 的 PassRatio=1 在 [1,1,.5,.5] 中 pct=75，AllPass 同 → correctness=75
	if !nearly(pa.Dims["correctness"].Score, 75) {
		t.Fatalf("A correctness 应 75, got %v", pa.Dims["correctness"].Score)
	}
	if !nearly(pb.Dims["correctness"].Score, 25) {
		t.Fatalf("B correctness 应 25, got %v", pb.Dims["correctness"].Score)
	}
	// 成本维（tokens/wall 低好）：A 两项都在基线低位 → 高分
	if pa.Dims["cost"].Score <= pb.Dims["cost"].Score {
		t.Fatalf("成本维方向错误: A=%v B=%v", pa.Dims["cost"], pb.Dims["cost"])
	}
}

// TestBuildProfileEdge 缺席规则与空窗口：无错误局调试维满分、无 edit 局
// 规划维零样本、空 own 窗口返回零值画像。
func TestBuildProfileEdge(t *testing.T) {
	noErr := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 2, Tokens: 10, WallMS: 10}
	p := BuildProfile("A", "t", []MatchMetrics{noErr, noErr}, []MatchMetrics{noErr, noErr}, 1)
	if d := p.Dims["debugging"]; d.Score != 100 || d.Raw != 1 {
		t.Fatalf("全无错误局调试维应满分: %+v", d)
	}
	if d := p.Dims["planning"]; d.Sample != 0 || d.Score != 0 {
		t.Fatalf("无 edit 局规划维应零样本零分: %+v", d)
	}

	empty := BuildProfile("A", "t", nil, nil, 1)
	if empty.SampleSize != 0 || empty.Dims != nil {
		t.Fatalf("空窗口应返回零值画像: %+v", empty)
	}

	// 崩溃局参与稳定性维：Crash 局在基线中低好 → 崩溃多的 agent 稳定性低分
	crash := MatchMetrics{Crash: true}
	pBad := BuildProfile("C", "t", []MatchMetrics{crash, crash}, []MatchMetrics{noErr, noErr, crash, crash}, 1)
	pGood := BuildProfile("D", "t", []MatchMetrics{noErr, noErr}, []MatchMetrics{noErr, noErr, crash, crash}, 1)
	if pBad.Dims["stability"].Score >= pGood.Dims["stability"].Score {
		t.Fatalf("崩溃 agent 稳定性应更低: bad=%v good=%v",
			pBad.Dims["stability"].Score, pGood.Dims["stability"].Score)
	}
}
