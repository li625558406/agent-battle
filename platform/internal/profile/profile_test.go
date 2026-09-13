// profile_test.go —— 归一化与聚合的边界测试。
package profile

import (
	"math"
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

// saneDims 断言所有已产出的维 Score 在 [0,100] 且非 NaN。
func saneDims(t *testing.T, p Profile) {
	t.Helper()
	for name, d := range p.Dims {
		if math.IsNaN(d.Score) {
			t.Fatalf("维 %s Score 为 NaN: %+v", name, d)
		}
		if d.Score < 0 || d.Score > 100 {
			t.Fatalf("维 %s Score 越界 [0,100]: %+v", name, d)
		}
	}
}

// TestBuildProfileAdversarial 三类恶意/极端输入的行为钉住：全崩溃窗口、
// 空基线、NaN 注入——均不得 panic、不得产出 NaN 或越界分。
func TestBuildProfileAdversarial(t *testing.T) {
	// a) 全崩溃窗口：正确性/成本/规划等维全部无有效样本，仅稳定性维参与
	// （Sample==2），其余维走缺席规则——聚合不得 panic。
	t.Run("all-crash window", func(t *testing.T) {
		crash := MatchMetrics{Crash: true}
		noErr := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 2, Tokens: 10, WallMS: 10}
		own := []MatchMetrics{crash, crash}
		baseline := []MatchMetrics{crash, noErr, crash, noErr}
		p := BuildProfile("A", "t", own, baseline, 1)
		st, ok := p.Dims["stability"]
		if !ok || st.Sample != 2 {
			t.Fatalf("稳定性维应存在且 Sample==2: %+v", p.Dims["stability"])
		}
		saneDims(t, p)
	})

	// b) 空基线 + 非空 own：空基线 pct 取中位 50 → 所有有样本维 Score==50
	// （debugging 除外：全无错误局走满分 100 特判，不属于百分位路径）。
	t.Run("empty baseline", func(t *testing.T) {
		ok := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 2, Tokens: 10, WallMS: 10}
		p := BuildProfile("A", "t", []MatchMetrics{ok, ok}, nil, 1)
		for name, d := range p.Dims {
			if name == "debugging" {
				continue
			}
			if d.Sample > 0 && d.Score != 50 {
				t.Fatalf("空基线下维 %s 应取中位 50, got %v", name, d.Score)
			}
		}
		saneDims(t, p)
	})

	// c) NaN 注入：基线混入 PassRatio=NaN 的局。pct 中 NaN 因 x<v 与 x==v
	// 同时为假而被静默不计数，等价于该基线值不存在于 less/eq 统计——行为
	// 钉住为"信任 ExtractMetrics 有界，导出入口对 NaN 基线不崩不越界"。
	t.Run("nan baseline injection", func(t *testing.T) {
		ok := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 2, Tokens: 10, WallMS: 10}
		nan := MatchMetrics{PassRatio: math.NaN()}
		baseline := []MatchMetrics{ok, ok, nan}
		p := BuildProfile("A", "t", []MatchMetrics{ok, ok}, baseline, 1)
		saneDims(t, p)
	})
}
