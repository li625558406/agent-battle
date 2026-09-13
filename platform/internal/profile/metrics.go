// metrics.go 从单局事件流与判分结果提取六维画像的原始指标（纯函数）。
// 口径见 docs/superpowers/specs/2026-09-13-m2-capability-profile-design.md §3。
package profile

import (
	"agentbattle/protocol"
)

// MatchMetrics 单局六维原始指标。布尔型参与标志（HasEvents/HasErrors/HasEdit/
// Crash）决定该局参与哪些维度的聚合——缺席局不计入对应维度样本。
type MatchMetrics struct {
	PassRatio float64 // passed/total；total==0 → 0
	AllPass   bool    // total>0 且全部通过

	HasErrors bool    // 局内含 error 事件（调试维的参与条件）
	Recovery  float64 // HasErrors 时的最终通过率（报错后恢复）

	ToolCalls int     // tool_call 事件数
	ErrRatio  float64 // error 事件数 / max(ToolCalls,1)（无效调用近似）；值域 [0,+∞) 可大于 1（error 事件可不伴随 tool_call），下游百分位为序数运算不受影响

	HasEdit bool    // 局内含 file_edit 事件（规划维的参与条件）
	PreEdit float64 // 首个 file_edit 前的 tool_call 数 / max(ToolCalls,1)

	Tokens    int   // 事件流 tokens 累加
	WallMS    int64 // 上报的耗时
	Crash     bool  // total==0（runner 契约：崩溃/超时侧按 0/0 上报）
	HasEvents bool  // 事件流可用（工具/成本维的参与条件）
}

// ExtractMetrics 提取单局指标。events 为 nil（事件流缺失或解压失败）时
// HasEvents=false；任何除零都有界（返回 0），不产生 NaN。
func ExtractMetrics(events []protocol.Event, passed, total int, wallMS int64) MatchMetrics {
	m := MatchMetrics{WallMS: wallMS, Crash: total == 0, HasEvents: len(events) > 0}
	if total > 0 {
		// 异常/恶意 runner 可能上报 passed<0 或 passed>total，钳制到 [0,total]
		// 保证 PassRatio（及复用它的 Recovery）恒在 [0,1]。
		p := passed
		if p < 0 {
			p = 0
		}
		if p > total {
			p = total
		}
		m.PassRatio = float64(p) / float64(total)
		m.AllPass = p == total
	}
	toolCalls, errEvents := 0, 0
	for _, e := range events {
		if e.Tokens > 0 {
			m.Tokens += e.Tokens
		}
		switch e.Type {
		case protocol.EventToolCall:
			toolCalls++
		case protocol.EventError:
			errEvents++
		case protocol.EventFileEdit:
			if !m.HasEdit {
				m.PreEdit = float64(toolCalls)
				m.HasEdit = true
			}
		}
	}
	m.ToolCalls = toolCalls
	den := toolCalls
	if den == 0 {
		den = 1
	}
	m.ErrRatio = float64(errEvents) / float64(den)
	m.PreEdit = m.PreEdit / float64(den)
	if errEvents > 0 {
		m.HasErrors = true
		m.Recovery = m.PassRatio
	}
	return m
}
