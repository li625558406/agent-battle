// profile.go 六维聚合与百分位归一化（纯函数）。
// 归一化基线 = 同 task_type 全体 agent 的近期对局池（调用方经 store 的
// ProfileBaseline 取得）；自身窗口决定参与局与样本量。
package profile

// 六维名称（Profile.Dims 的键）。
const (
	DimCorrect = "correctness"
	DimDebug   = "debugging"
	DimTool    = "tool_efficiency"
	DimCost    = "cost"
	DimPlan    = "planning"
	DimStab    = "stability"
)

// lowSampleThreshold 样本低于该值时画像标注 low_sample。
const lowSampleThreshold = 5

// Dim 单维：Score=归一化分（0-100），Raw=自身窗口主指标均值，Sample=参与局数。
type Dim struct {
	Score  float64 `json:"score"`
	Raw    float64 `json:"raw"`
	Sample int     `json:"sample"`
}

// Profile 完整画像。
type Profile struct {
	Agent      string         `json:"agent"`
	TaskType   string         `json:"task_type"`
	SampleSize int            `json:"sample_size"`
	LowSample  bool           `json:"low_sample"`
	Dims       map[string]Dim `json:"dims"`
	UpdatedAt  int64          `json:"updated_at"`
}

// pct 返回 v 在基线 vals 中的百分位（0-100）：
// (小于 v 的个数 + 0.5*等于 v 的个数)/n*100 —— 并列取中间档。
// 基线为空（新任务类型首批对局）时无信息可排名，一律取中位 50。
func pct(vals []float64, v float64) float64 {
	if len(vals) == 0 {
		return 50
	}
	less, eq := 0, 0
	for _, x := range vals {
		switch {
		case x < v:
			less++
		case x == v:
			eq++
		}
	}
	return (float64(less) + 0.5*float64(eq)) / float64(len(vals)) * 100
}

// collect 取基线中参与某指标的值集合。
func collect(ms []MatchMetrics, val func(MatchMetrics) (float64, bool)) []float64 {
	out := make([]float64, 0, len(ms))
	for _, m := range ms {
		if v, ok := val(m); ok {
			out = append(out, v)
		}
	}
	return out
}

// oneDim 计算单个指标维：own 中可参与的局逐个在基线取百分位（低好取反）
// 后求均值。无可参与局时 ok=false。
func oneDim(own, baseVals []float64, lowGood bool) (Dim, bool) {
	if len(own) == 0 {
		return Dim{}, false
	}
	var sum float64
	for _, v := range own {
		p := pct(baseVals, v)
		if lowGood {
			p = 100 - p
		}
		sum += p
	}
	return Dim{Score: sum / float64(len(own)), Sample: len(own)}, true
}

// mergeDim 合成一维的两指标分：只平均有样本的指标；都无样本返回零值。
func mergeDim(a, b Dim, raw float64) Dim {
	switch {
	case a.Sample > 0 && b.Sample > 0:
		return Dim{Score: (a.Score + b.Score) / 2, Raw: raw, Sample: a.Sample}
	case a.Sample > 0:
		return Dim{Score: a.Score, Raw: raw, Sample: a.Sample}
	case b.Sample > 0:
		return Dim{Score: b.Score, Raw: raw, Sample: b.Sample}
	}
	return Dim{}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// BuildProfile 聚合自身窗口 own 与全体基线 baseline，产出完整画像。
// own 为空时返回零值画像（Dims=nil，查询侧可区分"无对局"）。
func BuildProfile(agent, taskType string, own, baseline []MatchMetrics, now int64) Profile {
	p := Profile{Agent: agent, TaskType: taskType, SampleSize: len(own),
		LowSample: len(own) > 0 && len(own) < lowSampleThreshold, UpdatedAt: now}
	if len(own) == 0 {
		return p
	}
	p.Dims = map[string]Dim{}

	// 正确性：PassRatio + AllPass（均高好）
	passBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.PassRatio, true })
	allBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return b2f(m.AllPass), true })
	ownPass := collect(own, func(m MatchMetrics) (float64, bool) { return m.PassRatio, true })
	ownAll := collect(own, func(m MatchMetrics) (float64, bool) { return b2f(m.AllPass), true })
	d1, _ := oneDim(ownPass, passBase, false)
	d2, _ := oneDim(ownAll, allBase, false)
	rawPass := 0.0
	for _, m := range own {
		rawPass += m.PassRatio
	}
	p.Dims[DimCorrect] = mergeDim(d1, d2, rawPass/float64(len(own)))

	// 调试：Recovery（高好），仅 HasErrors 局；全窗无错误局 → 满分（无试错
	// 即无失败，视为该维无负担）
	hasErr := 0
	sumRec := 0.0
	for _, m := range own {
		if m.HasErrors {
			hasErr++
			sumRec += m.Recovery
		}
	}
	if hasErr == 0 {
		p.Dims[DimDebug] = Dim{Score: 100, Raw: 1, Sample: len(own)}
	} else {
		recBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.Recovery, m.HasErrors })
		ownRec := collect(own, func(m MatchMetrics) (float64, bool) { return m.Recovery, m.HasErrors })
		d, _ := oneDim(ownRec, recBase, false)
		p.Dims[DimDebug] = Dim{Score: d.Score, Raw: sumRec / float64(hasErr), Sample: hasErr}
	}

	// 工具效率：ToolCalls + ErrRatio（均低好），仅 HasEvents 局
	tcBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return float64(m.ToolCalls), m.HasEvents })
	erBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.ErrRatio, m.HasEvents })
	ownTC := collect(own, func(m MatchMetrics) (float64, bool) { return float64(m.ToolCalls), m.HasEvents })
	ownER := collect(own, func(m MatchMetrics) (float64, bool) { return m.ErrRatio, m.HasEvents })
	d1, _ = oneDim(ownTC, tcBase, true)
	d2, _ = oneDim(ownER, erBase, true)
	rawTC := 0.0
	for _, m := range own {
		if m.HasEvents {
			rawTC += float64(m.ToolCalls)
		}
	}
	p.Dims[DimTool] = mergeDim(d1, d2, rawTC/float64(max(1, countEvents(own))))

	// 成本：Tokens + WallMS（均低好），仅 HasEvents 局（无事件流时 tokens=0
	// 会假性最优，必须排除）
	tkBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return float64(m.Tokens), m.HasEvents })
	wlBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return float64(m.WallMS), m.HasEvents })
	ownTK := collect(own, func(m MatchMetrics) (float64, bool) { return float64(m.Tokens), m.HasEvents })
	ownWL := collect(own, func(m MatchMetrics) (float64, bool) { return float64(m.WallMS), m.HasEvents })
	d1, _ = oneDim(ownTK, tkBase, true)
	d2, _ = oneDim(ownWL, wlBase, true)
	p.Dims[DimCost] = mergeDim(d1, d2, 0)

	// 规划：PreEdit（高好），仅 HasEdit 局
	peBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.PreEdit, m.HasEdit })
	ownPE := collect(own, func(m MatchMetrics) (float64, bool) { return m.PreEdit, m.HasEdit })
	if d, ok := oneDim(ownPE, peBase, false); ok {
		rawPE := 0.0
		n := 0
		for _, m := range own {
			if m.HasEdit {
				rawPE += m.PreEdit
				n++
			}
		}
		p.Dims[DimPlan] = Dim{Score: d.Score, Raw: rawPE / float64(n), Sample: n}
	} else {
		p.Dims[DimPlan] = Dim{}
	}

	// 稳定性：Crash 0/1 + 通过率偏离窗口均值（均低好）
	crashBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return b2f(m.Crash), true })
	ownCrash := collect(own, func(m MatchMetrics) (float64, bool) { return b2f(m.Crash), true })
	mean := 0.0
	nTotal := 0
	for _, m := range own {
		if !m.Crash {
			mean += m.PassRatio
			nTotal++
		}
	}
	if nTotal > 0 {
		mean /= float64(nTotal)
	}
	devBase := collect(baseline, func(m MatchMetrics) (float64, bool) {
		return dev(m, mean), !m.Crash
	})
	ownDev := collect(own, func(m MatchMetrics) (float64, bool) {
		return dev(m, mean), !m.Crash
	})
	d1, _ = oneDim(ownCrash, crashBase, true)
	d2, _ = oneDim(ownDev, devBase, true)
	crashN := 0
	for _, m := range own {
		if m.Crash {
			crashN++
		}
	}
	p.Dims[DimStab] = mergeDim(d1, d2, float64(crashN)/float64(len(own)))
	return p
}

// dev 局通过率对参考均值的偏离（稳定性维的第二指标；崩溃局不参与）。
func dev(m MatchMetrics, mean float64) float64 {
	d := m.PassRatio - mean
	if d < 0 {
		return -d
	}
	return d
}

func countEvents(ms []MatchMetrics) int {
	n := 0
	for _, m := range ms {
		if m.HasEvents {
			n++
		}
	}
	return n
}
