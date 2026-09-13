// report_test.go —— BuildReport 时间线/标注/对比与降级、对抗性行为。
package review

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"testing"

	"agentbattle/protocol"
)

// gz 构造合法 gzip NDJSON 事件流（BuildReport 内部经 profile.DecodeEvents 解码）。
func gz(t *testing.T, events ...protocol.Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var meta = Meta{MatchID: 7, TaskID: "fix-add", TaskType: "general", Status: "done", Winner: "a"}

// TestBuildReportTimeline 正常流：时间线逐事件映射、first_error 标注、
// compare 各项（pass_ratio/tokens 复用 ExtractMetrics，errors/edits 自数）。
func TestBuildReportTimeline(t *testing.T) {
	a := SideInput{
		Agent: "echoA", Passed: 2, Total: 2, WallMS: 100,
		EventsGZ: gz(t,
			protocol.Event{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 5},
			protocol.Event{Seq: 2, Type: protocol.EventError},
			protocol.Event{Seq: 3, Type: protocol.EventFileEdit, Note: "src/a.go"},
		),
	}
	b := SideInput{
		Agent: "echoB", Passed: 1, Total: 2, WallMS: 200,
		EventsGZ: gz(t, protocol.Event{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 3}),
	}
	rep := BuildReport(meta, a, b)

	if rep.MatchID != 7 || rep.TaskID != "fix-add" || rep.TaskType != "general" ||
		rep.Status != "done" || rep.Winner != "a" {
		t.Fatalf("元信息不符: %+v", rep)
	}
	if sa := rep.Sides["a"]; sa.Agent != "echoA" || sa.Passed != 2 || sa.Total != 2 ||
		sa.WallMS != 100 || !sa.HasEvents {
		t.Fatalf("sides.a 不符: %+v", sa)
	}
	ta := rep.Timeline["a"]
	if len(ta) != 3 || ta[0].Seq != 1 || ta[0].Type != "tool_call" || ta[0].Tool != "bash" ||
		ta[0].Tokens != 5 || ta[2].Type != "file_edit" || ta[2].Path != "src/a.go" {
		t.Fatalf("timeline.a 不符: %+v", ta)
	}
	if len(rep.Marks["a"]) != 1 || rep.Marks["a"][0].Kind != "first_error" || rep.Marks["a"][0].Seq != 2 {
		t.Fatalf("marks.a 应为 first_error@2: %+v", rep.Marks["a"])
	}
	if len(rep.Marks["b"]) != 0 {
		t.Fatalf("b 无 error 应无标注: %+v", rep.Marks["b"])
	}
	c := rep.Compare
	if c.PassRatio.A != 1 || c.PassRatio.B != 0.5 ||
		c.ToolCalls.A != 1 || c.ToolCalls.B != 1 ||
		c.Errors.A != 1 || c.Errors.B != 0 ||
		c.Edits.A != 1 || c.Edits.B != 0 ||
		c.Tokens.A != 5 || c.Tokens.B != 3 ||
		c.WallMS.A != 100 || c.WallMS.B != 200 {
		t.Fatalf("compare 不符: %+v", c)
	}
}

// TestBuildReportDegraded 单侧事件流缺失/损坏：整侧降级（timeline 空、
// has_events=false），静态数据照常；空切片序列化为 [] 而非 null。
func TestBuildReportDegraded(t *testing.T) {
	rep := BuildReport(meta,
		SideInput{Agent: "a1", Passed: 2, Total: 2, WallMS: 100},
		SideInput{Agent: "b1", Passed: 1, Total: 2, WallMS: 200, EventsGZ: []byte("junk")},
	)
	sa := rep.Sides["a"]
	if sa.HasEvents || len(rep.Timeline["a"]) != 0 || len(rep.Marks["a"]) != 0 {
		t.Fatalf("无事件流侧应降级: %+v", sa)
	}
	if rep.Compare.PassRatio.A != 1 || rep.Compare.Tokens.A != 0 || rep.Compare.WallMS.A != 100 {
		t.Fatalf("降级侧静态数据应照常: %+v", rep.Compare)
	}
	if rep.Compare.Tokens.B != 0 {
		t.Fatalf("坏 gzip 侧 tokens 应 0: %+v", rep.Compare)
	}
	// 空切片必须序列化为 [] 而非 null（契约：前端/CLI 不处理 null 分支）
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"timeline":{"a":[],"b":[]}`)) ||
		!bytes.Contains(out, []byte(`"marks":{"a":[],"b":[]}`)) {
		t.Fatalf("空 timeline/marks 应序列化为 []:\n%s", out)
	}
}

// TestBuildReportAdversarial 对抗性输入全部有界：首事件即 error、全 error 流、
// passed 越界钳制、负 tokens 不计、NaN 不产生。
func TestBuildReportAdversarial(t *testing.T) {
	// 首事件即 error：标注其 seq
	rep := BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 2,
			EventsGZ: gz(t, protocol.Event{Seq: 9, Type: protocol.EventError})},
		SideInput{Agent: "b", Passed: 0, Total: 2})
	if mk := rep.Marks["a"]; len(mk) != 1 || mk[0].Seq != 9 {
		t.Fatalf("首事件即 error 应标注其 seq: %+v", mk)
	}
	// 全 error 流：errors=3、仅首个标注
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 0, Total: 0,
			EventsGZ: gz(t,
				protocol.Event{Seq: 1, Type: protocol.EventError},
				protocol.Event{Seq: 2, Type: protocol.EventError},
				protocol.Event{Seq: 3, Type: protocol.EventError})},
		SideInput{Agent: "b", Passed: 0, Total: 0})
	if rep.Compare.Errors.A != 3 {
		t.Fatalf("全 error 流 errors 应 3: %+v", rep.Compare)
	}
	if len(rep.Marks["a"]) != 1 || rep.Marks["a"][0].Seq != 1 {
		t.Fatalf("应仅标注首个 error: %+v", rep.Marks["a"])
	}
	if rep.Compare.PassRatio.A != 0 {
		t.Fatalf("total==0 → pass_ratio 0: %+v", rep.Compare)
	}
	// passed 越界钳制 + 负 tokens 不计（与 ExtractMetrics 口径一致）
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 5, Total: 2, WallMS: 1,
			EventsGZ: gz(t,
				protocol.Event{Seq: 1, Type: protocol.EventMessage, Tokens: -7},
				protocol.Event{Seq: 2, Type: protocol.EventMessage, Tokens: 4})},
		SideInput{Agent: "b", Passed: -1, Total: 2, WallMS: 1})
	if rep.Compare.PassRatio.A != 1 || rep.Compare.PassRatio.B != 0 {
		t.Fatalf("passed 越界应钳制到 [0,total]: %+v", rep.Compare)
	}
	if rep.Compare.Tokens.A != 4 {
		t.Fatalf("负 tokens 不应计入: %+v", rep.Compare)
	}
	// 任何 compare 值不得为 NaN
	out, _ := json.Marshal(rep.Compare)
	if bytes.Contains(out, []byte("NaN")) {
		t.Fatalf("compare 不得含 NaN: %s", out)
	}
	if math.IsNaN(rep.Compare.PassRatio.A) || math.IsNaN(rep.Compare.PassRatio.B) {
		t.Fatal("pass_ratio 不得为 NaN")
	}
}
