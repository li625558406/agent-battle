// metrics_test.go —— 单局指标提取的对抗性测试。
package profile

import (
	"math"
	"testing"

	"agentbattle/protocol"
)

// ev 构造带 Tokens 的事件。
func ev(typ string, tokens int) protocol.Event {
	return protocol.Event{Type: typ, Tokens: tokens}
}

// TestExtractMetricsNormal 常规局：2 工具调用（1 次报错）后修复 1 编辑并全部通过。
func TestExtractMetricsNormal(t *testing.T) {
	events := []protocol.Event{
		ev(protocol.EventToolCall, 100),
		ev(protocol.EventError, 0),
		ev(protocol.EventToolCall, 200),
		ev(protocol.EventFileEdit, 0),
		ev(protocol.EventResult, 0),
	}
	m := ExtractMetrics(events, 2, 2, 900)
	if !m.HasEvents || m.ToolCalls != 2 || m.Tokens != 300 {
		t.Fatalf("基础计数错误: %+v", m)
	}
	if !m.HasErrors || m.Recovery != 1.0 {
		t.Fatalf("报错恢复应 = 通过率 1.0: %+v", m)
	}
	if !m.HasEdit || m.PreEdit != 1.0 { // 首 edit 前 tool_call=2, 总=2 → 2/2
		t.Fatalf("前置探查比应 1.0: %+v", m)
	}
	if m.ErrRatio != 0.5 { // 1 error / 2 tool_call
		t.Fatalf("无效调用率应 0.5: %+v", m)
	}
	if m.PassRatio != 1.0 || !m.AllPass || m.Crash {
		t.Fatalf("正确性字段错误: %+v", m)
	}
}

// TestExtractMetricsDegenerate 对抗性：空流/零值/崩溃局/无 edit/全 error。
func TestExtractMetricsDegenerate(t *testing.T) {
	// 空事件流 + 崩溃局（total==0）：HasEvents=false、Crash=true，除零有界
	m := ExtractMetrics(nil, 0, 0, 123)
	if m.HasEvents || !m.Crash || m.PassRatio != 0 || m.ErrRatio != 0 {
		t.Fatalf("空流崩溃局应全零且有界: %+v", m)
	}
	// 有事件但零 tool_call 的 ErrRatio 与 PreEdit 不产生 NaN
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventError, 0), ev(protocol.EventFileEdit, 0)},
		0, 3, 10)
	if math.IsNaN(m.ErrRatio) || math.IsNaN(m.PreEdit) {
		t.Fatalf("不得产生 NaN: %+v", m)
	}
	if !m.HasErrors || !m.HasEdit {
		t.Fatalf("error/edit 应被识别: %+v", m)
	}
	if m.PreEdit != 0 { // 首 edit 前 tool_call=0
		t.Fatalf("无前置调用时 PreEdit 应 0: %+v", m)
	}
	// 全 error 事件、无 tool_call：ErrRatio 有界
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventError, 5), ev(protocol.EventError, 7)},
		0, 1, 1)
	if math.IsNaN(m.ErrRatio) || m.Tokens != 12 {
		t.Fatalf("全 error 局应有界且 tokens 累加: %+v", m)
	}
	// 无 edit 局：HasEdit=false
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventToolCall, 1)}, 1, 1, 1)
	if m.HasEdit {
		t.Fatalf("无 file_edit 局 HasEdit 应为 false: %+v", m)
	}
	// 超大 tokens 累加不溢出（int64 平台内 int 足够，只验证累加正确）
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventToolCall, math.MaxInt32), ev(protocol.EventToolCall, math.MaxInt32)}, 1, 1, 1)
	if m.Tokens != 2*math.MaxInt32 {
		t.Fatalf("tokens 应累加: %d", m.Tokens)
	}
}

// TestExtractMetricsAdversarial 对抗性：崩溃局中途留事件、多条 edit 只取首个、
// 畸形 passed（>total / 负数）钳制到 [0,total]。
func TestExtractMetricsAdversarial(t *testing.T) {
	// a) crash + 非空事件流：崩溃局中途留下事件的真是场景
	m := ExtractMetrics([]protocol.Event{ev(protocol.EventToolCall, 50), ev(protocol.EventError, 0)},
		0, 0, 100)
	if !m.Crash || !m.HasEvents || m.ToolCalls != 1 || !m.HasErrors || m.Tokens != 50 || m.AllPass {
		t.Fatalf("crash+事件流组合错误: %+v", m)
	}
	// b) 多条 file_edit 只取首个：首 edit 前 1 次 tool_call / 总 2 次
	m = ExtractMetrics([]protocol.Event{
		ev(protocol.EventToolCall, 0),
		ev(protocol.EventFileEdit, 0),
		ev(protocol.EventFileEdit, 0),
		ev(protocol.EventToolCall, 0),
	}, 1, 1, 10)
	if !m.HasEdit || m.PreEdit != 0.5 {
		t.Fatalf("应只取首个 edit 且 PreEdit=0.5: %+v", m)
	}
	// c) passed > total：钳制到 total，PassRatio=1.0 且 AllPass
	m = ExtractMetrics(nil, 5, 3, 10)
	if m.PassRatio != 1.0 || !m.AllPass {
		t.Fatalf("passed>total 应钳制为 1.0/AllPass: %+v", m)
	}
	// d) passed 为负：钳制到 0，PassRatio=0
	m = ExtractMetrics(nil, -2, 4, 10)
	if m.PassRatio != 0 {
		t.Fatalf("passed<0 应钳制为 0: %+v", m)
	}
}
