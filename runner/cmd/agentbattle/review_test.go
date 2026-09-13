// review_test.go —— CLI review 子命令的渲染与负路径测试。
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbattle/runner/internal/client"
)

// sampleReview 构造一份最小复盘报告。
func sampleReview() client.ReviewReport {
	return client.ReviewReport{
		MatchID: 7, TaskID: "fix-add", TaskType: "general", Status: "done", Winner: "a",
		Sides: map[string]client.ReviewSideOut{
			"a": {Agent: "echoA", Passed: 2, Total: 2, WallMS: 100, HasEvents: true},
			"b": {Agent: "echoB", Passed: 1, Total: 2, WallMS: 200, HasEvents: false},
		},
		Timeline: map[string][]client.ReviewTlEvent{
			"a": {
				{Seq: 1, Type: "tool_call", Tool: "bash", Tokens: 5},
				{Seq: 2, Type: "error"},
				{Seq: 3, Type: "file_edit", Path: "src/a.go", DurationMS: 12},
			},
			"b": {},
		},
		Marks: map[string][]client.ReviewMark{
			"a": {{Kind: "first_error", Seq: 2}},
			"b": {},
		},
		Compare: client.ReviewCompare{
			PassRatio: client.ReviewPair{A: 1, B: 0.5},
			ToolCalls: client.ReviewPair{A: 1, B: 0},
			Errors:    client.ReviewPair{A: 1, B: 0},
			Edits:     client.ReviewPair{A: 1, B: 0},
			Tokens:    client.ReviewPair{A: 5, B: 0},
			WallMS:    client.ReviewPair{A: 100, B: 200},
		},
	}
}

// TestPrintReview 渲染：胜者名、结论行、对比表、★标注、无事件侧 "—"
// 与"（无事件流）"提示、平局文案。
func TestPrintReview(t *testing.T) {
	var buf bytes.Buffer
	printReview(&buf, sampleReview())
	out := buf.String()
	for _, want := range []string{
		"对局 7", "fix-add", "general", "胜者 echoA",
		"通过率", "tool_call", "tokens", "wall_ms",
		"echoA", "echoB", "时间线", "#2 error", "★", "src/a.go",
		"（无事件流）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺 %q:\n%s", want, out)
		}
	}
	// 无事件侧的 event 类指标渲染 "—"：tool_call 行 A=1（有事件）、B=—（无事件流）
	found := false
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "tool_call") {
			if !strings.Contains(ln, "1") || !strings.Contains(ln, "—") {
				t.Fatalf("tool_call 行应含 A=1 与 B=—: %q", ln)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("输出缺 tool_call 行:\n%s", out)
	}

	// 平局：winner="tie" → 胜者显示 平局
	rep := sampleReview()
	rep.Winner = "tie"
	buf.Reset()
	printReview(&buf, rep)
	if !strings.Contains(buf.String(), "胜者 平局") {
		t.Fatalf("平局应显示 平局:\n%s", buf.String())
	}

	// 全零值报告（nil map）不 panic，渲染占位与平局文案
	buf.Reset()
	printReview(&buf, client.ReviewReport{})
	zero := buf.String()
	if !strings.Contains(zero, "（无事件流）") {
		t.Fatalf("零值报告应含（无事件流）:\n%s", zero)
	}
	if !strings.Contains(zero, "胜者 平局") {
		t.Fatalf("零值报告胜者应为 平局:\n%s", zero)
	}

	// 对抗性：agent 名含 ANSI ESC，三处出口（胜者行/概要行/时间线标题）均须剥控制字符
	evil := sampleReview()
	sa := evil.Sides["a"]
	sa.Agent = "evil\x1b[31mA"
	evil.Sides["a"] = sa
	buf.Reset()
	printReview(&buf, evil)
	eout := buf.String()
	if strings.ContainsRune(eout, '\x1b') {
		t.Fatalf("agent 名中的 ESC 转义应被剥除:\n%s", eout)
	}
	if !strings.Contains(eout, "evil") {
		t.Fatalf("净化后应保留剩余文本 evil:\n%s", eout)
	}
}

// TestCmdReviewNegative 必填校验与 404/409 错误透传。
func TestCmdReviewNegative(t *testing.T) {
	if err := cmdReview([]string{}); err == nil {
		t.Fatal("--server 缺失应报错")
	}
	if err := cmdReview([]string{"--server", "http://x"}); err == nil {
		t.Fatal("--match 缺失应报错")
	}
	if err := cmdReview([]string{"--server", "http://x", "--match", "0"}); err == nil {
		t.Fatal("--match 0 应报错")
	}
	if err := cmdReview([]string{"--server", "http://x", "--match", "-3"}); err == nil {
		t.Fatal("--match 负数应报错")
	}
	if err := cmdReview([]string{"--server", "http://x", "--match", "abc"}); err == nil {
		t.Fatal("--match 非数字应报错")
	}

	// 404 透传
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	if err := runReview(&buf, srv.URL, 99); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 应透传: %v", err)
	}

	// 正常路径：200 返回最小合法报告，渲染不报错
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"match_id":1,"task_id":"t","task_type":"general","status":"done",
			"winner":"a",
			"sides":{"a":{"agent":"A","passed":1,"total":1,"wall_ms":1,"has_events":false},
			         "b":{"agent":"B","passed":1,"total":1,"wall_ms":1,"has_events":false}},
			"timeline":{"a":[],"b":[]},"marks":{"a":[],"b":[]},
			"compare":{"pass_ratio":{"a":1,"b":1},"tool_calls":{"a":0,"b":0},
			           "errors":{"a":0,"b":0},"edits":{"a":0,"b":0},
			           "tokens":{"a":0,"b":0},"wall_ms":{"a":1,"b":1}}}`)
	}))
	defer ok.Close()
	buf.Reset()
	if err := runReview(&buf, ok.URL, 1); err != nil {
		t.Fatalf("正常复盘不应报错: %v", err)
	}
	if !strings.Contains(buf.String(), "对局 1") {
		t.Fatalf("应渲染结论行:\n%s", buf.String())
	}
}
