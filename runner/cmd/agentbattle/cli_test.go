// cli_test.go 覆盖 register/fetch/ladder 子命令与 mirror 联网上报核心逻辑。
// 用 httptest 模拟平台 API，对输出格式（E2E 会解析）与失败路径做断言。
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbattle/protocol"
	"agentbattle/runner/internal/client"
	"agentbattle/runner/internal/session"
)

// protocolReport 构造判分报告测试样本。
func protocolReport(passed, total int, hash string) protocol.JudgeReport {
	return protocol.JudgeReport{Passed: passed, Total: total, DiffHash: hash}
}

// fakePlatform 搭一个最小平台：可编程的对局/上报行为，并记录收到的请求体。
type fakePlatform struct {
	ts *httptest.Server

	matchBodies []map[string]string // POST /api/matches 请求体
	resultBodies []map[string]any   // POST /api/matches/{id}/results 请求体

	// handleResult 按调用次序返回预设响应（nil 项用默认 settled 响应）
	resultResps []map[string]any
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()
	fp := &fakePlatform{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/matches", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		fp.matchBodies = append(fp.matchBodies, b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"match_id":42,"judge_key":"k"}`)
	})
	mux.HandleFunc("POST /api/matches/{id}/results", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		fp.resultBodies = append(fp.resultBodies, b)
		resp := map[string]any{"status": "done", "winner": "a", "rating_a": 1100.0, "rating_b": 900.0}
		if i := len(fp.resultBodies) - 1; i < len(fp.resultResps) && fp.resultResps[i] != nil {
			resp = fp.resultResps[i]
		}
		bz, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bz)
	})
	fp.ts = httptest.NewServer(mux)
	t.Cleanup(fp.ts.Close)
	return fp
}

// TestRegisterOutputFormat：输出必须含一行 "token: <tok>"（E2E 解析锚点）
// 且含 agent 名与 id；token 不得因任何失败路径泄进错误信息。
func TestRegisterOutputFormat(t *testing.T) {
	var gotName string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotName = b["name"]
		fmt.Fprint(w, `{"id":7,"name":"my-agent","token":"tok-secret-123"}`)
	}))
	defer ts.Close()

	var buf bytes.Buffer
	if err := runRegister(&buf, ts.URL, "my-agent"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if gotName != "my-agent" {
		t.Fatalf("注册请求名不符: %q", gotName)
	}
	if !strings.Contains(out, "token: tok-secret-123") {
		t.Fatalf("输出缺少 token 行(E2E 锚点): %q", out)
	}
	if !strings.Contains(out, "my-agent") || !strings.Contains(out, "id=7") {
		t.Fatalf("输出缺少 agent 名/id: %q", out)
	}
}

// TestRegisterArgsValidation：缺 --server/--name 必须在发请求前被拦下。
func TestRegisterArgsValidation(t *testing.T) {
	if err := runRegister(&bytes.Buffer{}, "", "x"); err == nil {
		t.Fatal("空 server 必须报错")
	}
	if err := runRegister(&bytes.Buffer{}, "http://127.0.0.1:1", ""); err == nil {
		t.Fatal("空 name 必须报错")
	}
}

// TestFetchExtractsBundle：fetch 拉包解压到 --out 绝对路径，输出含任务名与目标路径。
func TestFetchExtractsBundle(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("task.json")
	f.Write([]byte(`{"task_id":"fix-add","name":"fix-add"}`))
	zw.Close()
	zipBytes := zipBuf.Bytes()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tasks/fix-add/bundle" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write(zipBytes)
	}))
	defer ts.Close()

	out := t.TempDir()
	var buf bytes.Buffer
	if err := runFetch(&buf, ts.URL, "fix-add", out); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(out)
	if !strings.Contains(buf.String(), fmt.Sprintf("任务包 fix-add 已解压到 %s", abs)) {
		t.Fatalf("输出格式不符: %q", buf.String())
	}
	b, err := os.ReadFile(filepath.Join(out, "task.json"))
	if err != nil || !strings.Contains(string(b), "fix-add") {
		t.Fatalf("解压产物缺失: %v %q", err, string(b))
	}
}

// TestLadderTableAndEmpty：正常榜按 "名称 评分 局数 胜 负 平" 打印；空榜打印提示。
func TestLadderTableAndEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ladder":[{"name":"A","rating":1100,"games":3,"wins":2,"losses":1,"ties":0},
			{"name":"B","rating":900,"games":3,"wins":1,"losses":2,"ties":0}]}`)
	}))
	defer ts.Close()
	var buf bytes.Buffer
	if err := runLadder(&buf, ts.URL, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"A", "B", "1100", "900"} {
		if !strings.Contains(out, want) {
			t.Fatalf("天梯输出缺少 %q: %q", want, out)
		}
	}
	if strings.Count(strings.TrimSpace(out), "\n") < 2 {
		t.Fatalf("应有两行数据: %q", out)
	}

	// 空榜：提示，nil 错误
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ladder":[]}`)
	}))
	defer ts2.Close()
	buf.Reset()
	if err := runLadder(&buf, ts2.URL, 0); err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(buf.String()); s == "" {
		t.Fatal("空榜必须有提示输出")
	}
}

// TestReportRoundSettled：每轮 = CreateMatch + A/B 两侧上报，断言请求体
// 字段（side/passed/total/wall_ms/diff_hash/events_gz）与结算打印。
func TestReportRoundSettled(t *testing.T) {
	fp := newFakePlatform(t)
	resA := session.Result{Report: protocolReport(2, 2, "hash-a"), WallMS: 120,
		Events: []protocol.Event{{Type: "tool_call"}, {Type: "result"}}}
	resB := session.Result{Report: protocolReport(1, 2, "hash-b"), WallMS: 130,
		Events: []protocol.Event{{Type: "tool_call"}}}

	var buf bytes.Buffer
	if err := reportRound(&buf, client.New(fp.ts.URL), 1,
		"tokA", "tokB", "fix-add", "alice", "bob", resA, resB, nil, nil); err != nil {
		t.Fatal(err)
	}

	if len(fp.matchBodies) != 1 {
		t.Fatalf("应创建 1 场对局, got %d", len(fp.matchBodies))
	}
	mb := fp.matchBodies[0]
	if mb["task_id"] != "fix-add" || mb["agent_a"] != "alice" || mb["agent_b"] != "bob" {
		t.Fatalf("CreateMatch 请求体不符: %v", mb)
	}
	if len(fp.resultBodies) != 2 {
		t.Fatalf("应上报两侧结果, got %d", len(fp.resultBodies))
	}
	a, b := fp.resultBodies[0], fp.resultBodies[1]
	if a["side"] != "a" || a["passed"] != float64(2) || a["total"] != float64(2) ||
		a["wall_ms"] != float64(120) || a["diff_hash"] != "hash-a" {
		t.Fatalf("A 侧上报体不符: %v", a)
	}
	if gz, ok := a["events_gz_base64"].(string); !ok || gz == "" {
		t.Fatalf("A 侧事件流缺失: %v", a)
	}
	if b["side"] != "b" || b["passed"] != float64(1) || b["diff_hash"] != "hash-b" {
		t.Fatalf("B 侧上报体不符: %v", b)
	}
	out := buf.String()
	if !strings.Contains(out, "第 1 轮 结算 winner=a A分=1100 B分=900") {
		t.Fatalf("结算打印不符: %q", out)
	}
}

// TestReportRoundWaiting：单侧到齐时平台返回 waiting，打印等待提示且不报错。
func TestReportRoundWaiting(t *testing.T) {
	fp := newFakePlatform(t)
	fp.resultResps = []map[string]any{{"status": "waiting"}, nil}
	res := session.Result{Report: protocolReport(1, 1, "h"), Events: []protocol.Event{{Type: "result"}}}
	var buf bytes.Buffer
	if err := reportRound(&buf, client.New(fp.ts.URL), 2,
		"tokA", "tokB", "fix-add", "a", "b", res, res, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "等待对方上报") {
		t.Fatalf("waiting 时应打印等待提示: %q", buf.String())
	}
}

// TestReportRoundErrorSideZeroUpload 对抗性用例：某侧 agent 崩溃（err 非 nil）
// 时仍须照常上报 0/0、空 diff_hash（技术性失败按零通过，双零平局规则兜底）。
func TestReportRoundErrorSideZeroUpload(t *testing.T) {
	fp := newFakePlatform(t)
	res := session.Result{Report: protocolReport(1, 1, "h"), Events: []protocol.Event{{Type: "result"}}}
	var buf bytes.Buffer
	if err := reportRound(&buf, client.New(fp.ts.URL), 1,
		"tokA", "tokB", "fix-add", "a", "b", session.Result{}, res, fmt.Errorf("agent 崩溃"), nil); err != nil {
		t.Fatal(err)
	}
	a := fp.resultBodies[0]
	if a["passed"] != float64(0) || a["total"] != float64(0) {
		t.Fatalf("崩溃侧应上报 0/0: %v", a)
	}
	// diff_hash 为空时被 omitempty 省略（服务端等价于空串），两者都算合规
	if h, ok := a["diff_hash"]; ok && h != "" {
		t.Fatalf("崩溃侧 diff_hash 必须为空: %v", a)
	}
	if _, ok := a["events_gz_base64"]; ok {
		t.Fatalf("崩溃侧无事件流, 不应带 events_gz: %v", a)
	}
}

// TestReportRoundUploadFailureAborts：B 侧上报失败必须返回 error（Report 钩子
// 语义：中止，不静默丢局）；错误信息不得包含 token。
func TestReportRoundUploadFailureAborts(t *testing.T) {
	// 只接受第 1 次（A 侧）上报，第 2 次（B 侧）返回 500
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/matches":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"match_id":9,"judge_key":"k"}`)
		case calls == 0:
			calls++
			fmt.Fprint(w, `{"status":"waiting"}`)
		default:
			http.Error(w, "内部错误", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()
	res := session.Result{Report: protocolReport(1, 1, "h")}
	err := reportRound(&bytes.Buffer{}, client.New(ts.URL), 1,
		"tokA", "tokB", "fix-add", "a", "b", res, res, nil, nil)
	if err == nil {
		t.Fatal("B 侧上报失败必须返回 error")
	}
	if strings.Contains(err.Error(), "tokA") || strings.Contains(err.Error(), "tokB") {
		t.Fatalf("错误信息不得泄漏 token: %v", err)
	}
}

// TestReportRoundCreateFailureAborts：CreateMatch 失败同样中止。
func TestReportRoundCreateFailureAborts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "服务器炸了", http.StatusInternalServerError)
	}))
	defer ts.Close()
	res := session.Result{Report: protocolReport(1, 1, "h")}
	if err := reportRound(&bytes.Buffer{}, client.New(ts.URL), 1,
		"tokA", "tokB", "fix-add", "a", "b", res, res, nil, nil); err == nil {
		t.Fatal("CreateMatch 失败必须返回 error")
	}
}
