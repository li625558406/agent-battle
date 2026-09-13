// Package review 是对局复盘报告的纯函数核心：输入单场对局的元信息与双侧
// 事件流/判分结果，组装结构化复盘（时间线 + 关键节点标注 + 行为对比）。
// 只依赖 protocol 与 profile（复用事件解码与指标提取，口径与画像单点一致），
// 不 import api/store。设计规格：
// docs/superpowers/specs/2026-09-13-m2-plan2-review-design.md §3。
package review

import (
	"agentbattle/platform/internal/profile"
	"agentbattle/protocol"
)

// Meta 对局元信息（api 层经 store.MatchByID/TaskTypeOf 读出后传入）。
type Meta struct {
	MatchID  int64
	TaskID   string
	TaskType string
	Status   string
	Winner   string
}

// SideInput 单侧输入（api 层经 store.ReviewData 读出后传入）。
type SideInput struct {
	Agent    string
	Passed   int
	Total    int
	WallMS   int64
	EventsGZ []byte
}

// SideMeta 单侧概要。
type SideMeta struct {
	Agent     string `json:"agent"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	WallMS    int64  `json:"wall_ms"`
	HasEvents bool   `json:"has_events"`
}

// TlEvent 时间线事件。红线 3：只含路径与操作类型（Path 来源事件 Note），
// 无文件内容明文。不含 PrevHash/Hash——哈希链属审计功能，M3+ 再做。
type TlEvent struct {
	Seq        int    `json:"seq"`
	TS         int64  `json:"ts"`
	Type       string `json:"type"`
	Tool       string `json:"tool,omitempty"`
	Path       string `json:"path,omitempty"` // 仅 file_edit 有值
	DurationMS int64  `json:"duration_ms,omitempty"`
	Tokens     int    `json:"tokens,omitempty"`
}

// Mark 关键节点标注。M2 标注集仅 first_error：首个 error 事件的 Seq。
// 设计文档 §6.4 的"大回滚"无事件语义（事件流无回滚/删除）、"转折点"无
// 规则口径，均砍掉；M3+ LLM judge 可接管。
type Mark struct {
	Kind string `json:"kind"`
	Seq  int    `json:"seq"`
}

// Pair 一项对比指标的 A/B 值。
type Pair struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

// Compare 双侧行为对比。pass_ratio/tool_calls/tokens/wall_ms 复用
// profile.ExtractMetrics（与画像同一份提取代码，口径不可能漂移）；
// errors/edits 为事件个数（时间线遍历时自数——MatchMetrics 只有布尔）。
type Compare struct {
	PassRatio Pair `json:"pass_ratio"`
	ToolCalls Pair `json:"tool_calls"`
	Errors    Pair `json:"errors"`
	Edits     Pair `json:"edits"`
	Tokens    Pair `json:"tokens"`
	WallMS    Pair `json:"wall_ms"`
}

// Report 复盘报告（GET /api/matches/{id}/review 返回体）。
type Report struct {
	MatchID  int64               `json:"match_id"`
	TaskID   string              `json:"task_id"`
	TaskType string              `json:"task_type"`
	Status   string              `json:"status"`
	Winner   string              `json:"winner"`
	Sides    map[string]SideMeta `json:"sides"`
	// Timeline/Marks 值恒为非 nil 切片（空侧序列化为 [] 非 null，消费端
	// 无需处理 null 分支）。
	Timeline map[string][]TlEvent `json:"timeline"`
	Marks    map[string][]Mark    `json:"marks"`
	Compare  Compare              `json:"compare"`
}

// sideStats 单侧内部统计（不导出：只服务于 Compare 组装）。
type sideStats struct {
	passRatio float64
	toolCalls int
	errors    int
	edits     int
	tokens    int
	wallMS    int64
}

// buildSide 单侧处理：解码事件（缺失/损坏整流降级）→ 一次遍历同时产出
// 时间线、first_error 标注与计数。
func buildSide(in SideInput) (SideMeta, []TlEvent, []Mark, sideStats) {
	events := profile.DecodeEvents(in.EventsGZ)
	m := profile.ExtractMetrics(events, in.Passed, in.Total, in.WallMS)
	meta := SideMeta{
		Agent: in.Agent, Passed: in.Passed, Total: in.Total,
		WallMS: in.WallMS, HasEvents: len(events) > 0,
	}
	tl := make([]TlEvent, 0, len(events))
	marks := []Mark{}
	st := sideStats{
		passRatio: m.PassRatio, toolCalls: m.ToolCalls,
		tokens: m.Tokens, wallMS: in.WallMS,
	}
	firstErr := true
	for _, e := range events {
		te := TlEvent{Seq: e.Seq, TS: e.TS, Type: e.Type, Tool: e.Tool}
		if e.Type == protocol.EventFileEdit {
			te.Path = e.Note
			st.edits++
		}
		if e.Tokens > 0 {
			te.Tokens = e.Tokens
		}
		te.DurationMS = e.DurationMS
		tl = append(tl, te)
		if e.Type == protocol.EventError {
			st.errors++
			if firstErr {
				marks = append(marks, Mark{Kind: "first_error", Seq: e.Seq})
				firstErr = false
			}
		}
	}
	return meta, tl, marks, st
}

// BuildReport 组装复盘报告。单侧事件流缺失/解码失败时该侧降级：timeline
// 空、has_events=false，静态数据（pass_ratio/wall_ms）照常——与画像管道
// 降级口径一致。
func BuildReport(meta Meta, a, b SideInput) Report {
	sa, ta, ma, sta := buildSide(a)
	sb, tb, mb, stb := buildSide(b)
	return Report{
		MatchID: meta.MatchID, TaskID: meta.TaskID, TaskType: meta.TaskType,
		Status: meta.Status, Winner: meta.Winner,
		Sides:    map[string]SideMeta{"a": sa, "b": sb},
		Timeline: map[string][]TlEvent{"a": ta, "b": tb},
		Marks:    map[string][]Mark{"a": ma, "b": mb},
		Compare: Compare{
			PassRatio: Pair{A: sta.passRatio, B: stb.passRatio},
			ToolCalls: Pair{A: float64(sta.toolCalls), B: float64(stb.toolCalls)},
			Errors:    Pair{A: float64(sta.errors), B: float64(stb.errors)},
			Edits:     Pair{A: float64(sta.edits), B: float64(stb.edits)},
			Tokens:    Pair{A: float64(sta.tokens), B: float64(stb.tokens)},
			WallMS:    Pair{A: float64(sta.wallMS), B: float64(stb.wallMS)},
		},
	}
}
