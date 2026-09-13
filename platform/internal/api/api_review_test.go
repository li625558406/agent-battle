// api_review_test.go —— GET /api/matches/{id}/review 三分支与内容断言。
package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agentbattle/protocol"
)

// gzEventsB64 构造 gzip NDJSON 事件流并 base64（上报 body 用）。
func gzEventsB64(t *testing.T, events ...protocol.Event) string {
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
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

type reviewResp struct {
	MatchID  int64  `json:"match_id"`
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
	Status   string `json:"status"`
	Winner   string `json:"winner"`
	Sides    map[string]struct {
		Agent     string `json:"agent"`
		Passed    int    `json:"passed"`
		Total     int    `json:"total"`
		WallMS    int64  `json:"wall_ms"`
		HasEvents bool   `json:"has_events"`
	} `json:"sides"`
	Timeline map[string][]struct {
		Seq  int    `json:"seq"`
		Type string `json:"type"`
		Path string `json:"path"`
	} `json:"timeline"`
	Marks map[string][]struct {
		Kind string `json:"kind"`
		Seq  int    `json:"seq"`
	} `json:"marks"`
	Compare map[string]struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	} `json:"compare"`
}

// TestReviewFlow 已结算对局复盘内容断言 + 400/404/409（pending、aborted）分支。
func TestReviewFlow(t *testing.T) {
	srv, st := newServerWithStore(t)

	// 一场带事件流的对局：A 有 tool_call+error+file_edit，B 仅 tool_call
	tokA := register(t, srv, "ra")
	tokB := register(t, srv, "rb")
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "ra", "agent_b": "rb"})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局: %d", resp.StatusCode)
	}
	mid := int64(m["match_id"].(float64))
	up := func(tok, side string, passed int, events []protocol.Event) {
		t.Helper()
		body := map[string]any{"side": side, "passed": passed, "total": 2, "wall_ms": 100}
		if events != nil {
			body["events_gz_base64"] = gzEventsB64(t, events...)
		}
		resp, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid), tok, body)
		if resp.StatusCode != 200 {
			t.Fatalf("上报 %s: %d", side, resp.StatusCode)
		}
	}
	up(tokA, "a", 2, []protocol.Event{
		{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 5},
		{Seq: 2, Type: protocol.EventError},
		{Seq: 3, Type: protocol.EventFileEdit, Note: "src/a.go"},
	})
	up(tokB, "b", 1, []protocol.Event{
		{Seq: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 3},
	})

	// 200：完整内容断言
	resp, m = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid), "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("复盘查询: %d", resp.StatusCode)
	}
	b, _ := json.Marshal(m)
	var rr reviewResp
	if err := json.Unmarshal(b, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.MatchID != mid || rr.TaskID != "demo" || rr.TaskType != "general" ||
		rr.Status != "done" || rr.Winner != "a" {
		t.Fatalf("元信息不符: %+v", rr)
	}
	if sa := rr.Sides["a"]; sa.Agent != "ra" || sa.Passed != 2 || !sa.HasEvents {
		t.Fatalf("sides.a 不符: %+v", sa)
	}
	if ta := rr.Timeline["a"]; len(ta) != 3 || ta[2].Path != "src/a.go" {
		t.Fatalf("timeline.a 不符: %+v", ta)
	}
	if tb := rr.Timeline["b"]; len(tb) != 1 {
		t.Fatalf("timeline.b 不符: %+v", tb)
	}
	if mk := rr.Marks["a"]; len(mk) != 1 || mk[0].Kind != "first_error" || mk[0].Seq != 2 {
		t.Fatalf("marks.a 不符: %+v", mk)
	}
	if len(rr.Marks["b"]) != 0 {
		t.Fatalf("marks.b 应空: %+v", rr.Marks["b"])
	}
	if rr.Compare["errors"].A != 1 || rr.Compare["edits"].A != 1 ||
		rr.Compare["tool_calls"].B != 1 || rr.Compare["tokens"].A != 5 ||
		rr.Compare["pass_ratio"].A != 1 || rr.Compare["pass_ratio"].B != 0.5 {
		t.Fatalf("compare 不符: %+v", rr.Compare)
	}

	// 400：matchID 非数字
	if resp, _ = do(t, "GET", srv.URL+"/api/matches/abc/review", "", nil); resp.StatusCode != 400 {
		t.Fatalf("非数字 id 应 400, got %d", resp.StatusCode)
	}
	// 404：对局不存在
	if resp, _ = do(t, "GET", fmt.Sprintf("%s/api/matches/99999/review", srv.URL), "", nil); resp.StatusCode != 404 {
		t.Fatalf("不存在对局应 404, got %d", resp.StatusCode)
	}

	// 经 API 的对抗上报：负 wall_ms / 越界 passed 进报告应被守卫
	_, m3 := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "ra", "agent_b": "rb"})
	mid3 := int64(m3["match_id"].(float64))
	up3 := func(tok, side string, body map[string]any) {
		t.Helper()
		resp, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid3), tok, body)
		if resp.StatusCode != 200 {
			t.Fatalf("对抗上报 %s: %d", side, resp.StatusCode)
		}
	}
	up3(tokA, "a", map[string]any{"side": "a", "passed": -5, "total": 0, "wall_ms": -1})
	up3(tokB, "b", map[string]any{"side": "b", "passed": 1, "total": 2, "wall_ms": 50})
	resp, m = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid3), "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("对抗对局复盘: %d", resp.StatusCode)
	}
	b, _ = json.Marshal(m)
	var rr3 reviewResp
	if err := json.Unmarshal(b, &rr3); err != nil {
		t.Fatal(err)
	}
	if rr3.Compare["pass_ratio"].A != 0 || rr3.Compare["wall_ms"].A != 0 {
		t.Fatalf("对抗静态字段应被守卫: %+v", rr3.Compare)
	}

	// 409 × 2：pending 与 aborted
	_, m2 := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "ra", "agent_b": "rb"})
	mid2 := int64(m2["match_id"].(float64))
	resp2, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid2), tokA,
		map[string]any{"side": "a", "passed": 1, "total": 2, "wall_ms": 1})
	if resp2.StatusCode != 200 {
		t.Fatalf("pending 对局单侧上报: %d", resp2.StatusCode)
	}
	if resp, _ = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid2), "", nil); resp.StatusCode != 409 {
		t.Fatalf("pending 对局应 409, got %d", resp.StatusCode)
	}
	if _, err := st.SweepStaleMatches(-time.Minute); err != nil { // 负阈值清扫一切 pending → aborted
		t.Fatal(err)
	}
	if resp, _ = do(t, "GET", fmt.Sprintf("%s/api/matches/%d/review", srv.URL, mid2), "", nil); resp.StatusCode != 409 {
		t.Fatalf("aborted 对局应 409, got %d", resp.StatusCode)
	}
}
