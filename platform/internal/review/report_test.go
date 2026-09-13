// report_test.go —— BuildReport 时间线/标注/对比与降级、对抗性行为。
package review

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"strings"
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
	// 任何 compare 值不得为 NaN（json.Marshal 遇 NaN 报错，以此钉住）
	if _, err := json.Marshal(rep.Compare); err != nil {
		t.Fatalf("compare 序列化失败（含 NaN?）: %v", err)
	}
	if math.IsNaN(rep.Compare.PassRatio.A) || math.IsNaN(rep.Compare.PassRatio.B) {
		t.Fatal("pass_ratio 不得为 NaN")
	}
	// file_edit 的 Note 为空：Path 应被 omitempty 省略（不出现空串键）
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 1,
			EventsGZ: gz(t, protocol.Event{Seq: 1, Type: protocol.EventFileEdit})},
		SideInput{Agent: "b", Passed: 1, Total: 1})
	if ta := rep.Timeline["a"]; len(ta) != 1 || ta[0].Path != "" {
		t.Fatalf("空 Note 的 file_edit Path 应为空: %+v", ta)
	}
	if out, err := json.Marshal(rep); err != nil || bytes.Contains(out, []byte(`"path":""`)) {
		t.Fatalf("空 path 应被 omitempty 省略: err=%v out=%s", err, out)
	}
	// 乱序/重复 Seq：时间线按流序镜像（不做排序/去重），钉住该语义
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 1,
			EventsGZ: gz(t,
				protocol.Event{Seq: 5, Type: protocol.EventToolCall, Tool: "bash"},
				protocol.Event{Seq: 2, Type: protocol.EventToolCall, Tool: "bash"},
				protocol.Event{Seq: 5, Type: protocol.EventError})},
		SideInput{Agent: "b", Passed: 1, Total: 1})
	ta := rep.Timeline["a"]
	if len(ta) != 3 || ta[0].Seq != 5 || ta[1].Seq != 2 || ta[2].Seq != 5 {
		t.Fatalf("时间线应按流序镜像不排序不去重: %+v", ta)
	}
	if mk := rep.Marks["a"]; len(mk) != 1 || mk[0].Seq != 5 {
		t.Fatalf("乱序流中首个 error 的 seq 应为 5: %+v", mk)
	}
	// 负 DurationMS 不透传（与 Tokens 守卫同口径）
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 1,
			EventsGZ: gz(t,
				protocol.Event{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", DurationMS: -99},
				protocol.Event{Seq: 2, Type: protocol.EventToolCall, Tool: "bash", DurationMS: 7})},
		SideInput{Agent: "b", Passed: 1, Total: 1})
	if ta := rep.Timeline["a"]; len(ta) != 2 || ta[0].DurationMS != 0 || ta[1].DurationMS != 7 {
		t.Fatalf("负 DurationMS 应丢弃、正值保留: %+v", ta)
	}
	// 超长 Note 钳制到 1024
	long := strings.Repeat("x", 2000)
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 1,
			EventsGZ: gz(t, protocol.Event{Seq: 1, Type: protocol.EventFileEdit, Note: long})},
		SideInput{Agent: "b", Passed: 1, Total: 1})
	if ta := rep.Timeline["a"]; len(ta) != 1 || len(ta[0].Path) != 1024 {
		t.Fatalf("超长 Note 应钳制到 1024: got %d", len(rep.Timeline["a"][0].Path))
	}
	// 时间线条数上限：compare/标注按全量算，timeline 截到 maxTimeline
	var many []protocol.Event
	for i := 0; i < 3000; i++ {
		many = append(many, protocol.Event{Seq: i + 1, Type: protocol.EventToolCall, Tool: "bash"})
	}
	many = append(many, protocol.Event{Seq: 3001, Type: protocol.EventError})
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 1, EventsGZ: gz(t, many...)},
		SideInput{Agent: "b", Passed: 1, Total: 1})
	if len(rep.Timeline["a"]) != maxTimeline {
		t.Fatalf("timeline 应截断到 %d: got %d", maxTimeline, len(rep.Timeline["a"]))
	}
	if rep.Compare.ToolCalls.A != 3000 || rep.Compare.Errors.A != 1 {
		t.Fatalf("compare 应按全量事件计算: %+v", rep.Compare)
	}
	if mk := rep.Marks["a"]; len(mk) != 1 || mk[0].Seq != 3001 {
		t.Fatalf("截断窗口外的 first_error 仍应标注: %+v", mk)
	}
	// Tool 超长钳制 + 未知 Type 归一 other
	rep = BuildReport(meta,
		SideInput{Agent: "a", Passed: 1, Total: 1,
			EventsGZ: gz(t,
				protocol.Event{Seq: 1, Type: "weird-type", Tool: strings.Repeat("t", 300)},
				protocol.Event{Seq: 2, Type: protocol.EventToolCall, Tool: "go"})},
		SideInput{Agent: "b", Passed: 1, Total: 1})
	ta = rep.Timeline["a"]
	if ta[0].Type != "other" || len(ta[0].Tool) != 128 {
		t.Fatalf("未知 Type 应归一 other、Tool 应钳 128: %+v", ta[0])
	}
}
