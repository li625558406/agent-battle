package api

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 故障注入测试用裸连接直改库文件

	"agentbattle/platform/internal/store"
)

func newServerWithStore(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	return newServerWithStoreAt(t, filepath.Join(t.TempDir(), "api.db"))
}

// newServerWithStoreAt 允许指定库路径：故障注入测试需要裸连接直改同一库文件。
func newServerWithStoreAt(t *testing.T, dbPath string) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(dbPath)
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

// ---------- M2 画像：task_type / 结算钩子 / 查询路由 ----------

// profileResp 是 GET /api/agents/{name}/profile 的响应形态。
type profileResp struct {
	Agent    string `json:"agent"`
	Profiles []struct {
		TaskType    string `json:"task_type"`
		SampleSize  int    `json:"sample_size"`
		ProfileJSON string `json:"profile_json"`
		UpdatedAt   int64  `json:"updated_at"`
	} `json:"profiles"`
}

// playMatch 经 HTTP 注册两名 agent、创建对局、双侧上报（A 全过 / B 半过）
// 触发结算，返回双方 token。
func playMatch(t *testing.T, srv *httptest.Server, nameA, nameB string) (tokA, tokB string) {
	t.Helper()
	tokA = register(t, srv, nameA)
	tokB = register(t, srv, nameB)
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": nameA, "agent_b": nameB})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局: %d", resp.StatusCode)
	}
	mid := int64(m["match_id"].(float64))
	up := func(tok string, side string, passed int) {
		t.Helper()
		body := map[string]any{"side": side, "passed": passed, "total": 2, "wall_ms": 100}
		if side == "a" {
			body["events_gz_base64"] = base64.StdEncoding.EncodeToString([]byte{})
		}
		resp, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid), tok, body)
		if resp.StatusCode != 200 {
			t.Fatalf("上报 %s: %d", side, resp.StatusCode)
		}
	}
	up(tokA, "a", 2)
	up(tokB, "b", 1)
	return tokA, tokB
}

// TestProfileFlow 结算后画像落库、路由可查、404/空画像三分支。
func TestProfileFlow(t *testing.T) {
	srv, _ := newServerWithStore(t)
	playMatch(t, srv, "pa", "pb")

	resp, m := do(t, "GET", srv.URL+"/api/agents/pa/profile", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("查画像: %d", resp.StatusCode)
	}
	b, _ := json.Marshal(m)
	var pr profileResp
	if err := json.Unmarshal(b, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Agent != "pa" || len(pr.Profiles) != 1 {
		t.Fatalf("应 1 条画像: %+v", pr)
	}
	prof := pr.Profiles[0]
	if prof.TaskType != "general" { // demo task.json 无 task_type → 归一
		t.Fatalf("task_type 应归一 general: %+v", prof)
	}
	if prof.SampleSize != 1 || !strings.Contains(prof.ProfileJSON, `"correctness"`) {
		t.Fatalf("画像内容不符: %+v", prof)
	}

	// 未知 agent → 404；已知 agent 无对局 → 200 空数组
	if resp, _ = do(t, "GET", srv.URL+"/api/agents/ghost/profile", "", nil); resp.StatusCode != 404 {
		t.Fatalf("未知 agent 应 404, got %d", resp.StatusCode)
	}
	register(t, srv, "pc")
	resp, _ = do(t, "GET", srv.URL+"/api/agents/pc/profile", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("无对局 agent 应 200, got %d", resp.StatusCode)
	}
}

// TestProfileKnownDifferenceAPI A 全过 vs B 半过 → A 的 correctness 分严格
// 高于 B（归一化基线全体池语义在 API 层的验收）。
func TestProfileKnownDifferenceAPI(t *testing.T) {
	srv, _ := newServerWithStore(t)
	playMatch(t, srv, "ka", "kb")

	get := func(name string) float64 {
		t.Helper()
		_, m := do(t, "GET", srv.URL+"/api/agents/"+name+"/profile", "", nil)
		b, _ := json.Marshal(m)
		var pr profileResp
		if err := json.Unmarshal(b, &pr); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Dims map[string]struct {
				Score float64 `json:"score"`
			} `json:"dims"`
		}
		if err := json.Unmarshal([]byte(pr.Profiles[0].ProfileJSON), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Dims["correctness"].Score
	}
	sa, sb := get("ka"), get("kb")
	if sa <= sb {
		t.Fatalf("画像应复现已知差异: A=%v B=%v", sa, sb)
	}
}

// TestReadTaskType 归一化分支全覆盖：读失败/畸形 JSON/类型不符/空白 → general。
func TestReadTaskType(t *testing.T) {
	dir := t.TempDir()
	write := func(taskID, content string) {
		t.Helper()
		d := filepath.Join(dir, taskID)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "task.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 目录不存在
	if got := readTaskType(dir, "ghost"); got != "general" {
		t.Fatalf("目录不存在应归一 general, got %q", got)
	}
	// 畸形 JSON
	write("broken", "{not json")
	if got := readTaskType(dir, "broken"); got != "general" {
		t.Fatalf("畸形 JSON 应归一 general, got %q", got)
	}
	// task_type 类型不符
	write("wrongtype", `{"task_type": 123}`)
	if got := readTaskType(dir, "wrongtype"); got != "general" {
		t.Fatalf("类型不符应归一 general, got %q", got)
	}
	// 空值与纯空白
	write("empty", `{"task_type": ""}`)
	write("blank", `{"task_type": "   "}`)
	for _, id := range []string{"empty", "blank"} {
		if got := readTaskType(dir, id); got != "general" {
			t.Fatalf("%s 空白应归一 general, got %q", id, got)
		}
	}
	// 正常读取（含两侧空白裁剪）
	write("real", `{"task_type": "debug"}`)
	if got := readTaskType(dir, "real"); got != "debug" {
		t.Fatalf("正常读取失败: %q", got)
	}
	write("padded", `{"task_type": "  spaced  "}`)
	if got := readTaskType(dir, "padded"); got != "spaced" {
		t.Fatalf("空白裁剪失败: %q", got)
	}
}

// TestRecomputeFailureDoesNotBlockSettlement 故障注入：画像落库表被毁时
// 结算响应不受影响（重算失败仅 log 降级，规格 §5/§6 承诺）。
// 用裸连接 DROP agent_profiles → 结算钩子里 Recompute 的 UpsertProfile 必败，
// 断言结算照常完成（Elo/战绩落库）、响应正常；再重建空表验证画像确无写入。
func TestRecomputeFailureDoesNotBlockSettlement(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "broken.db")
	srv, _ := newServerWithStoreAt(t, dbPath)

	// 第二条裸连接直改同一库文件（DSN 形态以 store.Open 为准，路径转 /）
	dbsql, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer dbsql.Close()
	if _, err := dbsql.Exec(`DROP TABLE agent_profiles`); err != nil {
		t.Fatal(err)
	}

	playMatch(t, srv, "fa", "fb") // 内部断言双侧上报均 200

	// 结算确实完成且正确：天梯可见 Elo 1220/1180（首局 K=40）与战绩，
	// 证明画像重算失败没有阻断结算事务
	_, m := do(t, "GET", srv.URL+"/api/ladder", "", nil)
	rows, _ := m["ladder"].([]any)
	if len(rows) != 2 {
		t.Fatalf("天梯行数 = %d, 期望 2", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	if first["name"] != "fa" || first["rating"].(float64) != 1220 || first["wins"].(float64) != 1 {
		t.Fatalf("结算数据异常（重算失败不应影响结算）: %v", first)
	}

	// 重建空表后查询画像：upsert 失败 → 无任何画像写入，应为 200 空数组
	if _, err := dbsql.Exec(`CREATE TABLE agent_profiles (
		agent_id     INTEGER NOT NULL REFERENCES agents(id),
		task_type    TEXT NOT NULL DEFAULT 'general',
		sample_size  INTEGER NOT NULL DEFAULT 0,
		profile_json TEXT NOT NULL,
		updated_at   INTEGER NOT NULL,
		PRIMARY KEY (agent_id, task_type)
	)`); err != nil {
		t.Fatal(err)
	}
	resp, m := do(t, "GET", srv.URL+"/api/agents/fa/profile", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("重算失败后画像查询应 200, got %d", resp.StatusCode)
	}
	b, _ := json.Marshal(m)
	var pr profileResp
	if err := json.Unmarshal(b, &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Profiles) != 0 {
		t.Fatalf("表被毁期间结算不应写入任何画像: %+v", pr)
	}
}
