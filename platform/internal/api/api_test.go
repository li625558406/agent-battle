package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentbattle/platform/internal/store"
)

func newServerWithStore(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tasksDir := t.TempDir()
	// 伪造一个最小任务目录
	if err := os.MkdirAll(filepath.Join(tasksDir, "demo", "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, "demo", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "demo", "task.json"),
		[]byte(`{"task_id":"demo","name":"d","description":"x","timeout_sec":60}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "demo", "tests", "manifest.json"),
		[]byte(`{"task_id":"demo","tests":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "demo", "tests", "sig"), []byte("sig"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, tasksDir, []byte("dev-secret")))
	t.Cleanup(srv.Close)
	return srv, st
}

func newServer(t *testing.T) *httptest.Server {
	srv, _ := newServerWithStore(t)
	return srv
}

// do 发起请求并读空 body；返回响应（含状态码/头）与解析后的 JSON（若为 JSON）。
func do(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("响应 JSON 解析失败: %v（body=%s）", err, b)
		}
	}
	return resp, m
}

// register 注册一个 agent 并返回其 token。
func register(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	resp, m := do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": name})
	if resp.StatusCode != 201 {
		t.Fatalf("注册 %s: 状态码 = %d, 期望 201", name, resp.StatusCode)
	}
	tok, _ := m["token"].(string)
	if tok == "" {
		t.Fatalf("注册 %s: 响应缺少 token: %v", name, m)
	}
	return tok
}

func TestRegisterAgent(t *testing.T) {
	srv := newServer(t)

	// 正常注册：201 且返回 id/name/token
	resp, m := do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": "alpha"})
	if resp.StatusCode != 201 {
		t.Fatalf("状态码 = %d, 期望 201", resp.StatusCode)
	}
	if m["name"] != "alpha" || m["token"] == "" {
		t.Fatalf("响应字段缺失: %v", m)
	}
	// 契约：id 必须是 JSON 数字（客户端按 int64 解析）
	if id, ok := m["id"].(float64); !ok || id < 1 {
		t.Fatalf("注册响应 id = %v, 期望正数数字", m["id"])
	}

	// 重名：409
	resp, _ = do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": "alpha"})
	if resp.StatusCode != 409 {
		t.Fatalf("重名状态码 = %d, 期望 409", resp.StatusCode)
	}

	// 空名：400
	resp, _ = do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": ""})
	if resp.StatusCode != 400 {
		t.Fatalf("空名状态码 = %d, 期望 400", resp.StatusCode)
	}

	// 缺 name 字段：400
	resp, _ = do(t, "POST", srv.URL+"/api/agents", "", map[string]any{})
	if resp.StatusCode != 400 {
		t.Fatalf("缺 name 状态码 = %d, 期望 400", resp.StatusCode)
	}

	// 畸形 JSON：400
	req, _ := http.NewRequest("POST", srv.URL+"/api/agents", strings.NewReader("{not json"))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("畸形 JSON 状态码 = %d, 期望 400", resp2.StatusCode)
	}

	// 超长 name（1MB）：400，不能进库
	req, _ = http.NewRequest("POST", srv.URL+"/api/agents",
		strings.NewReader(`{"name":"`+strings.Repeat("x", 1<<20)+`"}`))
	resp2, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("超长 body 状态码 = %d, 期望 400", resp2.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	srv := newServer(t)
	body := map[string]any{"task_id": "demo", "agent_a": "x", "agent_b": "y"}

	// 无 token：401
	resp, _ := do(t, "POST", srv.URL+"/api/matches", "", body)
	if resp.StatusCode != 401 {
		t.Fatalf("无 token 状态码 = %d, 期望 401", resp.StatusCode)
	}

	// 坏 token：401
	resp, _ = do(t, "POST", srv.URL+"/api/matches", "deadbeef", body)
	if resp.StatusCode != 401 {
		t.Fatalf("坏 token 状态码 = %d, 期望 401", resp.StatusCode)
	}

	// token 属于别的 agent 也不能注册重名……（此处仅验证认证层，业务校验在 MatchFlow）
}

func TestMatchFlow(t *testing.T) {
	srv := newServer(t)
	tokA := register(t, srv, "A")
	tokB := register(t, srv, "B")

	// 任务目录不存在：400
	resp, _ := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "nope", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 400 {
		t.Fatalf("任务不存在状态码 = %d, 期望 400", resp.StatusCode)
	}

	// agent 名不存在：400
	resp, _ = do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A", "agent_b": "ghost"})
	if resp.StatusCode != 400 {
		t.Fatalf("agent 不存在状态码 = %d, 期望 400", resp.StatusCode)
	}

	// 自我对局（agent_a == agent_b）：400（对抗性：污染战绩归属）
	resp, _ = do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A", "agent_b": "A"})
	if resp.StatusCode != 400 {
		t.Fatalf("自我对局状态码 = %d, 期望 400", resp.StatusCode)
	}

	// task_id 路径穿越：400（对抗性）
	resp, _ = do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "../etc", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 400 {
		t.Fatalf("路径穿越 task_id 状态码 = %d, 期望 400", resp.StatusCode)
	}

	// 字段缺失：400
	resp, _ = do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A"})
	if resp.StatusCode != 400 {
		t.Fatalf("字段缺失状态码 = %d, 期望 400", resp.StatusCode)
	}

	// 正常创建：201 + match_id + judge_key
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局状态码 = %d, 期望 201", resp.StatusCode)
	}
	// 契约：match_id 必须是 JSON 数字（客户端按 int64 解析）
	matchIDf, ok := m["match_id"].(float64)
	if !ok || m["judge_key"] == "" {
		t.Fatalf("创建对局响应缺少 match_id/judge_key: %v", m)
	}
	matchID := strconv.FormatInt(int64(matchIDf), 10)

	// 同侧重复上报应被拒绝（对抗性：幂等/重复触发）
	resBody := func(passed int) map[string]any {
		return map[string]any{
			"side": "a", "passed": passed, "total": 10,
			"wall_ms": 1000, "diff_hash": "h", "events_gz_base64": "",
		}
	}

	// 无关 token 上报：403
	tokC := register(t, srv, "C")
	resp, _ = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokC, resBody(8))
	if resp.StatusCode != 403 {
		t.Fatalf("无关 token 状态码 = %d, 期望 403", resp.StatusCode)
	}

	// 非法 side：400
	resp, _ = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokA,
		map[string]any{"side": "z", "passed": 8, "total": 10, "wall_ms": 1000})
	if resp.StatusCode != 400 {
		t.Fatalf("非法 side 状态码 = %d, 期望 400", resp.StatusCode)
	}

	// match id 非数字：400
	resp, _ = do(t, "POST", srv.URL+"/api/matches/abc/results", tokA, resBody(8))
	if resp.StatusCode != 400 {
		t.Fatalf("非数字 match id 状态码 = %d, 期望 400", resp.StatusCode)
	}

	// 不存在的 match id：404
	resp, _ = do(t, "POST", srv.URL+"/api/matches/9999/results", tokA, resBody(8))
	if resp.StatusCode != 404 {
		t.Fatalf("不存在 match id 状态码 = %d, 期望 404", resp.StatusCode)
	}

	// A 上报：waiting
	resp, m = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokA, resBody(8))
	if resp.StatusCode != 200 || m["status"] != "waiting" {
		t.Fatalf("A 上报: %d %v, 期望 200 waiting", resp.StatusCode, m)
	}

	// A 同侧重复上报：被主键拒绝，应 409，不能改写结果
	resp, m = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokA, resBody(9))
	if resp.StatusCode != 409 {
		t.Fatalf("同侧重复上报状态码 = %d, 期望 409", resp.StatusCode)
	}
	// 错误文案必须是固定提示，不得回显 SQLite 内部错误（约束名/路径泄漏）
	if s, _ := m["error"].(string); s != "该侧结果已上报过" {
		t.Fatalf("同侧重复上报错误文案 = %q, 期望固定文案", s)
	}

	// B 上报：done，winner=a，Elo 1220/1180（双方 1200 起、首局 K=40）
	resp, m = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokB,
		map[string]any{"side": "b", "passed": 6, "total": 10,
			"wall_ms": 2000, "diff_hash": "h2", "events_gz_base64": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("B 上报状态码 = %d, 期望 200", resp.StatusCode)
	}
	if m["status"] != "done" || m["winner"] != "a" {
		t.Fatalf("B 上报: %v, 期望 done/winner=a", m)
	}
	if ra, ok := m["rating_a"].(float64); !ok || ra != 1220 {
		t.Fatalf("rating_a = %v, 期望 1220", m["rating_a"])
	}
	if rb, ok := m["rating_b"].(float64); !ok || rb != 1180 {
		t.Fatalf("rating_b = %v, 期望 1180", m["rating_b"])
	}

	// 天梯：2 行，第一行 A
	resp, m = do(t, "GET", srv.URL+"/api/ladder", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("天梯状态码 = %d, 期望 200", resp.StatusCode)
	}
	rows, _ := m["ladder"].([]any)
	if len(rows) != 3 {
		t.Fatalf("天梯行数 = %d, 期望 3（A/B/C，C 因 403 用例注册）", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	if first["name"] != "A" {
		t.Fatalf("天梯第一名 = %v, 期望 A", first["name"])
	}
	if first["rating"].(float64) != 1220 || first["wins"].(float64) != 1 {
		t.Fatalf("第一名数据异常: %v", first)
	}
	// 天梯响应不得泄露 token（对抗性：敏感字段）
	if _, has := first["token"]; has {
		t.Fatal("天梯响应包含 token 字段，泄露凭证")
	}
	if _, has := m["token"]; has {
		t.Fatal("天梯响应顶层包含 token 字段")
	}
}

// TestResultRejectedAfterSweep 验证 API 层拒绝对 aborted 对局上报（409 固定文案）。
func TestResultRejectedAfterSweep(t *testing.T) {
	srv, st := newServerWithStore(t)
	tokA := register(t, srv, "A")
	register(t, srv, "B") // B 需存在才能创建对局；其 token 在本用例中不使用
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局状态码 = %d, 期望 201", resp.StatusCode)
	}
	matchID := strconv.FormatInt(int64(m["match_id"].(float64)), 10)

	// 负阈值：cutoff 在未来，全部 pending 命中 → 该对局被置为 aborted
	if _, err := st.SweepStaleMatches(-time.Minute); err != nil {
		t.Fatal(err)
	}

	resp, m = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokA,
		map[string]any{"side": "a", "passed": 1, "total": 1, "wall_ms": 100})
	if resp.StatusCode != 409 {
		t.Fatalf("aborted 对局上报状态码 = %d, 期望 409", resp.StatusCode)
	}
	if s, _ := m["error"].(string); s != "对局已结束，拒绝上报" {
		t.Fatalf("aborted 上报错误文案 = %q, 期望固定文案", s)
	}
}

func TestTaskBundle(t *testing.T) {
	srv := newServer(t)

	resp, err := http.Get(srv.URL + "/api/tasks/demo/bundle")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bundle 状态码 = %d, 期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, 期望 application/zip", ct)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("响应不是合法 zip: %v", err)
	}
	want := map[string]bool{
		"task.json":            false,
		"tests/manifest.json": false,
		"tests/sig":           false,
	}
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("zip 缺少条目 %s；实际条目: %v", name, zipNames(zr))
		}
	}

	// 不存在的任务：404
	resp2, err := http.Get(srv.URL + "/api/tasks/nope/bundle")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("不存在任务 bundle 状态码 = %d, 期望 404", resp2.StatusCode)
	}

	// 路径穿越：404（对抗性，不得读 tasksDir 之外的文件）
	for _, evil := range []string{"..%2F..%2Fetc", "%2e%2e"} {
		r, err := http.Get(srv.URL + "/api/tasks/" + evil + "/bundle")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Fatalf("穿越路径 %q bundle 状态码 = %d, 期望 404", evil, r.StatusCode)
		}
	}
}

func zipNames(zr *zip.Reader) []string {
	var out []string
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}
