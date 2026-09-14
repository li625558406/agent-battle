# M2 计划 3：--dry-run 影子赛 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `mirror --dry-run` 完整走镜像对战流程但零出网——所有本应发给平台的请求经本地回环假服务端原样落盘，用户逐字节核查"平台会收到什么"。

**Architecture:** 新包 `runner/internal/dryrun` 承载全部影子赛逻辑（回环假服务端 + dump 落盘 + waiting/done 编排 + 事件流解码附加）；`cmdMirror` 只加 `--dry-run` 旗标接线（互斥校验、代发两次注册、跳过天梯拉取、stdout 摘要与隐私声明），主流程一字不改。

**Tech Stack:** Go 标准库（net/http Go1.22 路由模式、compress/gzip、encoding/json）；测试 httptest 风格 + 黑盒 E2E（exec 真实二进制）。

**规格:** `docs/superpowers/specs/2026-09-14-m2-plan3-dryrun-design.md`（含计划期修订 ef0a3e1：register 由接线代发、无 bundle 路由）

---

## 工程须知（每个任务都适用）

1. **gopls 滞后误报是本仓库长期现象**：IDE 报 undefined/UnusedImport/语法错误一律忽略（已发生 10 次，全部为误报），以真实命令为准：`go build ./... && go vet ./... && go test -race -count=1 <目标包>`，每次都亲自跑。
2. **提交规范**：中文消息，`feat:`/`test:`/`fix:`/`docs:` 前缀，末尾 `Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>`（HEREDOC 传消息）。只 commit 不 push。只 add 任务列出的文件。
3. **internal 边界**：`runner/internal/` 子树内各包可互相导入（如 dryrun 可导入 `agentbattle/runner/internal/client` 与 `agentbattle/protocol`），但 runner 与 platform 互不可见。
4. **Windows git-bash 环境**：路径用 forward slash；测试里的 os.MkdirAll/WriteFile 均为跨平台标准库调用，无需特殊处理。
5. **对抗性测试要求**：每个任务的测试都要有"试图让代码出错"的用例（全局 CLAUDE.md 规则），不能只测 happy path。

## File Structure

| 文件 | 职责 |
|---|---|
| `runner/internal/dryrun/server.go`（新建） | 假服务端全部逻辑：生命周期、三路由编排、dump 落盘、事件流解码 |
| `runner/internal/dryrun/server_test.go`（新建，T1-T3 逐步填充） | 编排正确性、dump 保真、对抗用例 |
| `runner/cmd/agentbattle/mirror.go`（修改） | `--dry-run` 旗标、互斥校验、代发注册、stdout 摘要与隐私声明、天梯跳过 |
| `runner/cmd/agentbattle/mirror_test.go`（新建） | CLI 负路径（互斥/缺名/离线流程可达） |
| `runner/cmd/agentbattle/main.go`（修改） | usage 文案补 `--dry-run` |
| `runner/e2e/platform_e2e_test.go`（修改） | `TestDryRunMirror` 黑盒 E2E |
| `CHANGE.md` / `CLAUDE.md`（修改，T5） | 收官记录 |

---

### Task 1: dryrun 包骨架——生命周期 + register/create_match 路由 + dump 保真

**Files:**
- Create: `runner/internal/dryrun/server.go`
- Test: `runner/internal/dryrun/server_test.go`

- [ ] **Step 1: 写失败测试（骨架部分：register/create_match 编排、dump 保真、Records 顺序、NewServer 目录冲突）**

创建 `runner/internal/dryrun/server_test.go`：

```go
// server_test.go —— dryrun 假服务端：编排正确性、dump 保真与对抗用例。
package dryrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agentbattle/runner/internal/client"
)

// newTestServer 起一个导出到 t.TempDir 的假服务端，返回 server 与输出目录。
func newTestServer(t *testing.T, nameA, nameB string) (*Server, string) {
	t.Helper()
	out := t.TempDir()
	s, err := NewServer(Options{OutDir: out, NameA: nameA, NameB: nameB})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, out
}

// readDump 读一个导出文件并解析为 dumpFile。
func readDump(t *testing.T, outDir, name string) dumpFile {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(outDir, "dry_run", name))
	if err != nil {
		t.Fatalf("读导出文件 %s 失败: %v", name, err)
	}
	var df dumpFile
	if err := json.Unmarshal(bs, &df); err != nil {
		t.Fatalf("解析导出文件 %s 失败: %v\n%s", name, err, bs)
	}
	return df
}

// TestRegister 编排与落盘：A/B 名命中分侧文件、占位 token、未知名无后缀。
func TestRegister(t *testing.T) {
	s, out := newTestServer(t, "echoA", "echoB")
	cl := client.New(s.Base())

	id, tok, err := cl.Register("echoA")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 || tok != placeholder {
		t.Fatalf("register 编排不符: id=%d tok=%q", id, tok)
	}
	df := readDump(t, out, "001_register_a.json")
	if df.Method != "POST" || df.Path != "/api/agents" {
		t.Fatalf("register dump 方法/路径不符: %s %s", df.Method, df.Path)
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(df.Body, &body); err != nil || body.Name != "echoA" {
		t.Fatalf("register dump body 不符: %q %v", body.Name, err)
	}

	// B 侧：文件名带 _b 后缀、id 递增
	if _, tok, err := cl.Register("echoB"); err != nil || tok != placeholder {
		t.Fatalf("register B 失败: %v %q", err, tok)
	}
	readDump(t, out, "002_register_b.json")

	// 未知名（既非 nameA 也非 nameB）：仍落盘，无后缀
	if _, _, err := cl.Register("stranger"); err != nil {
		t.Fatal(err)
	}
	readDump(t, out, "003_register.json")

	recs := s.Records()
	if len(recs) != 3 || recs[0].Seq != 1 || recs[2].Seq != 3 {
		t.Fatalf("Records 顺序不符: %+v", recs)
	}
	for _, r := range recs {
		if r.Method != "POST" || !strings.HasPrefix(r.File, fmt.Sprintf("%03d_", r.Seq)) {
			t.Fatalf("Record 字段不符: %+v", r)
		}
	}
}

// TestDumpFidelity 保真：自定义 header 与原始 body 字节逐字一致。
func TestDumpFidelity(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	raw := `{"name":"A","extra":[1,2,3]}`
	req, err := http.NewRequest(http.MethodPost, s.Base()+"/api/agents",
		bytes.NewReader([]byte(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test", "hello")
	resp, err := s.srv.Handler.(*noMux) // 占位，Step 3 实现时删除此行改用真实 client
	_ = resp
	cl := client.New(s.Base())
	if _, _, err := cl.Register("A"); err != nil {
		t.Fatal(err)
	}
	_ = req
	_ = out
}
```

**注意**：`TestDumpFidelity` 上面骨架里的 `noMux` 占位是**故意写错**（先红设计）；Step 3 实现后用下面 Step 2 的最终版替换整个函数。为避免困惑，直接在 Step 2 落最终测试版。

- [ ] **Step 2: 把 TestDumpFidelity 替换为最终版（dump 保真断言）**

```go
// TestDumpFidelity 保真：自定义 header 与原始 body 字节逐字一致。
func TestDumpFidelity(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	raw := []byte(`{"name":"A","extra":[1,2,3]}`)
	req, err := http.NewRequest(http.MethodPost, s.Base()+"/api/agents", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test", "hello")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	df := readDump(t, out, "001_register_a.json")
	if string(df.Body) != string(raw) {
		t.Fatalf("body 不保真:\nwant %s\ngot  %s", raw, df.Body)
	}
	if df.Headers["X-Test"] != "hello" {
		t.Fatalf("自定义 header 丢失: %v", df.Headers)
	}
	if df.Headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type 丢失: %v", df.Headers)
	}
}

// TestNonJSONBody 非法 JSON body：不崩，body 以 JSON 字符串形式如实记录。
func TestNonJSONBody(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	resp, err := http.Post(s.Base()+"/api/agents", "text/plain", strings.NewReader("not json \x01"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	df := readDump(t, out, "001_register.json")
	if !strings.Contains(string(df.Body), "not json") {
		t.Fatalf("非法 JSON body 应被字符串化记录: %s", df.Body)
	}
}

// TestNewServerDirConflict OutDir 是已存在的普通文件时启动报错。
func TestNewServerDirConflict(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(Options{OutDir: file, NameA: "A", NameB: "B"}); err == nil {
		t.Fatal("OutDir 为普通文件应报错")
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test -race ./runner/internal/dryrun/`
Expected: 编译失败（`NewServer`/`Options`/`placeholder`/`dumpFile` undefined——包尚不存在，需先建空包占位：可在 server.go 先写 `package dryrun` 一行让它编译到具体 undefined 错误）。

- [ ] **Step 4: 实现 server.go 骨架**

创建 `runner/internal/dryrun/server.go`：

```go
// Package dryrun 实现 --dry-run 影子赛的本地回环假服务端：把 mirror 流程
// 本应发给平台的请求原样落盘，并应答预编排的最小合法响应，让用户在零出网
// 前提下逐字节核查平台将收到的数据（隐私承诺，设计文档 §7.3）。
package dryrun

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// placeholder 是假服务端返回的凭证字面量：导出文件里的 X-Token 展示的
// 就是它的位置——真实对局该处是真实 token。
const placeholder = "dry-run-placeholder"

// maxBody 单请求 body 帽（10MB）：mirror 流程的正常请求远小于此，
// 触帽即异常（如事件流被撑爆），fail fast 不落盘。
const maxBody = 10 << 20

// Options 假服务端配置。
type Options struct {
	OutDir string // dump 根目录（其下创建 dry_run 子目录）
	NameA  string // A 侧注册名（用于 register_a/_b 文件标注）
	NameB  string
}

// Record 一条被拦截请求的摘要（CLI 展示用）。
type Record struct {
	Seq    int    `json:"seq"`
	Method string `json:"method"`
	Path   string `json:"path"`
	File   string `json:"file"`
	Note   string `json:"note"`
}

// dumpFile 导出文件结构：平台将收到的全部数据。
type dumpFile struct {
	Method            string            `json:"method"`
	Path              string            `json:"path"`
	Headers           map[string]string `json:"headers"`
	Body              json.RawMessage   `json:"body"`
	Events            []json.RawMessage `json:"events,omitempty"`
	EventsDecodeError string            `json:"events_decode_error,omitempty"`
}

// resultSide 记录一侧上报的通过数（双侧到齐后判 winner 用）。
type resultSide struct {
	Passed, Total int
}

// Server 本地回环假服务端。
type Server struct {
	ln  net.Listener
	srv *http.Server
	dir string

	nameA, nameB string

	mu      sync.Mutex
	seq     int
	records []Record
	agentID int64
	matchID int64
	sides   map[int64]map[string]resultSide // matchID → 已到齐侧
	dumpErr error                           // 首次 dump 写盘失败（CLI 收尾时中止）
}

// NewServer 启动假服务端，绑 127.0.0.1 随机端口。
func NewServer(opts Options) (*Server, error) {
	dir := filepath.Join(opts.OutDir, "dry_run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 dry_run 导出目录失败: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("绑定本地回环端口失败: %w", err)
	}
	s := &Server{
		ln: ln, dir: dir,
		nameA: opts.NameA, nameB: opts.NameB,
		sides: map[int64]map[string]resultSide{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents", s.handleAgent)
	mux.HandleFunc("POST /api/matches", s.handleCreateMatch)
	mux.HandleFunc("POST /api/matches/{id}/results", s.handleResult)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln) // 生命周期由 Close 收尾（测试/CLI 同步退出，无需优雅关停）
	return s, nil
}

// Base 返回 client 接线用的根地址。
func (s *Server) Base() string { return "http://" + s.ln.Addr().String() }

// Close 关停假服务端。
func (s *Server) Close() error { return s.srv.Close() }

// Records 返回至今全部拦截记录（时序序）。
func (s *Server) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

// DumpError 返回首次 dump 写盘失败（调用方应中止并报错）。
func (s *Server) DumpError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dumpErr
}

// ---- 路由与编排 ----

// handleAgent 编排 POST /api/agents：自增 id + 占位凭证。
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	bs, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(bs, &req) // 畸形 body → Name 空，仍如实落盘
	side := ""
	switch req.Name {
	case s.nameA:
		side = "a"
	case s.nameB:
		side = "b"
	}
	s.mu.Lock()
	s.agentID++
	resp := map[string]any{"id": s.agentID, "name": req.Name, "token": placeholder}
	s.dumpLocked(r, bs, "register"+sideSuffix(side), "name="+req.Name)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateMatch 编排 POST /api/matches：自增 match_id。
func (s *Server) handleCreateMatch(w http.ResponseWriter, r *http.Request) {
	bs, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		TaskID string `json:"task_id"`
		AgentA string `json:"agent_a"`
		AgentB string `json:"agent_b"`
	}
	_ = json.Unmarshal(bs, &req)
	s.mu.Lock()
	s.matchID++
	resp := map[string]any{"match_id": s.matchID, "judge_key": placeholder}
	s.dumpLocked(r, bs, "create_match", "task_id="+req.TaskID)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// handleResult 编排 POST /api/matches/{id}/results（Task 2 实现完整版：
// 先固定返回 waiting 让骨架可编译运行）。
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	if _, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err != nil {
		http.Error(w, "match id 非数字", http.StatusNotFound)
		return
	}
	bs, ok := s.readBody(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	s.dumpLocked(r, bs, "upload", "")
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
}

// ---- dump 落盘 ----

// readBody 读请求体（10MB 帽，触帽 413 不落盘）。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	bs, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "请求体超过 10MB 帽", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return bs, true
}

// sideSuffix 分侧文件后缀；未知侧无后缀。
func sideSuffix(side string) string {
	if side == "a" || side == "b" {
		return "_" + side
	}
	return ""
}

// dumpLocked 以通用结构落盘（调用方必须持有 mu）。
func (s *Server) dumpLocked(r *http.Request, body []byte, slug, note string) {
	s.dumpFileLocked(r, newDumpFile(r, body), slug, note)
}

// dumpFileLocked 序号命名落盘并登记 Record；写盘失败记入 dumpErr（CLI 收尾中止）。
func (s *Server) dumpFileLocked(r *http.Request, df *dumpFile, slug, note string) {
	s.seq++
	name := fmt.Sprintf("%03d_%s.json", s.seq, slug)
	b, err := json.MarshalIndent(df, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(s.dir, name), b, 0o644)
	}
	if err != nil && s.dumpErr == nil {
		s.dumpErr = fmt.Errorf("写导出文件 %s 失败: %w", name, err)
		note = "dump 失败: " + err.Error()
	}
	s.records = append(s.records, Record{
		Seq: s.seq, Method: r.Method, Path: r.URL.Path, File: name, Note: note,
	})
}

// newDumpFile 组装导出结构：headers 展平；body 合法 JSON 原样嵌入、
// 非法 JSON 字符串化（保证导出文件永远是合法 JSON）。
func newDumpFile(r *http.Request, body []byte) *dumpFile {
	hdrs := map[string]string{}
	for k, v := range r.Header {
		hdrs[k] = strings.Join(v, ",")
	}
	b := bytes.TrimLeft(body, " \t\r\n")
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') && json.Valid(b) {
		return &dumpFile{Method: r.Method, Path: r.URL.Path,
			Headers: hdrs, Body: json.RawMessage(b)}
	}
	q, err := json.Marshal(string(body))
	if err != nil {
		q = []byte(`"<marshal fallback failed>"`)
	}
	return &dumpFile{Method: r.Method, Path: r.URL.Path,
		Headers: hdrs, Body: json.RawMessage(q)}
}

// decodeEvents 解码 events_gz_base64（base64 → gunzip → NDJSON）为事件数组；
// 任一步失败返回错误，调用方记入 events_decode_error 不中断流程。
func decodeEvents(b64 string) ([]json.RawMessage, error) {
	gz, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("base64 解码失败: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("gzip 解压失败: %w", err)
	}
	defer zr.Close()
	dec := json.NewDecoder(zr)
	var out []json.RawMessage
	for {
		var rm json.RawMessage
		if err := dec.Decode(&rm); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("事件流 NDJSON 解析失败: %w", err)
		}
		out = append(out, rm)
	}
}

// writeJSON 统一 JSON 应答。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 5: 运行测试转绿**

Run: `go build ./... && go vet ./... && go test -race -count=1 ./runner/internal/dryrun/`
Expected: 全部 PASS（TestRegister / TestDumpFidelity / TestNonJSONBody / TestNewServerDirConflict）。

- [ ] **Step 6: Commit**

```bash
git add runner/internal/dryrun/server.go runner/internal/dryrun/server_test.go
git commit -m "$(cat <<'EOF'
feat: dryrun 包骨架——回环假服务端、register/create_match 编排与 dump 保真落盘

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: upload 路由——waiting/done 编排 + winner 判定 + 事件流解码附加

**Files:**
- Modify: `runner/internal/dryrun/server.go`（handleResult 替换为完整版 + resultReq 类型）
- Test: `runner/internal/dryrun/server_test.go`（末尾追加）

- [ ] **Step 1: 写失败测试（末尾追加）**

```go
// uploadSample 构造合法的 events_gz_base64（gzip NDJSON 两个事件）。
func uploadSample(t *testing.T) string {
	t.Helper()
	gz, err := client.GzipEvents([]protocol.Event{
		{Seq: 1, TS: 1, Type: protocol.EventToolCall, Tool: "bash", Tokens: 5},
		{Seq: 2, TS: 2, Type: protocol.EventError},
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(gz)
}

// upload 用裸 HTTP 上报一测（直接控制 body JSON）。
func upload(t *testing.T, base string, matchID int64, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(fmt.Sprintf("%s/api/matches/%d/results", base, matchID),
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decodeResp 解析响应 JSON 到 map。
func decodeResp(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	return out
}

// TestUploadOrchestration 首侧 waiting、双侧 done 且 winner 按通过率判。
func TestUploadOrchestration(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	b64 := uploadSample(t)

	r1 := decodeResp(t, upload(t, s.Base(), 1,
		fmt.Sprintf(`{"side":"a","passed":2,"total":2,"wall_ms":100,"events_gz_base64":%q}`, b64)))
	if r1["status"] != "waiting" {
		t.Fatalf("首侧应 waiting: %v", r1)
	}
	r2 := decodeResp(t, upload(t, s.Base(), 1, `{"side":"b","passed":1,"total":2,"wall_ms":200}`))
	if r2["status"] != "done" || r2["winner"] != "a" {
		t.Fatalf("双侧到齐应 done/winner=a: %v", r2)
	}
	if r2["rating_a"] != 1200.0 || r2["rating_b"] != 1200.0 {
		t.Fatalf("rating 编排不符: %v", r2)
	}

	// tie：两边同比例
	upload(t, s.Base(), 2, `{"side":"a","passed":1,"total":2,"wall_ms":50}`)
	r4 := decodeResp(t, upload(t, s.Base(), 2, `{"side":"b","passed":1,"total":2,"wall_ms":999}`))
	if r4["winner"] != "tie" {
		t.Fatalf("同比例应 tie（影子赛不比时长）: %v", r4)
	}

	// 导出文件：upload_a 附加解码后事件、upload_b 无 events 字段
	dfa := readDump(t, out, "004_upload_a.json")
	if len(dfa.Events) != 2 {
		t.Fatalf("upload_a 应附加 2 条事件: %d", len(dfa.Events))
	}
	if !bytes.Contains(dfa.Events[1], []byte(`"error"`)) {
		t.Fatalf("事件内容不符: %s", dfa.Events[1])
	}
	dfb := readDump(t, out, "005_upload_b.json")
	if len(dfb.Events) != 0 || dfb.EventsDecodeError != "" {
		t.Fatalf("upload_b 不应有 events 字段: %+v", dfb)
	}
}

// TestUploadEventsDecodeError 畸形 events_gz_base64：降级标注不中断。
func TestUploadEventsDecodeError(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	r := decodeResp(t, upload(t, s.Base(), 1,
		`{"side":"a","passed":0,"total":2,"events_gz_base64":"!!!"}`))
	if r["status"] != "waiting" {
		t.Fatalf("解码失败不应中断编排: %v", r)
	}
	df := readDump(t, out, "001_upload_a.json")
	if df.EventsDecodeError == "" {
		t.Fatalf("应记录 events_decode_error: %+v", df)
	}
}
```

需要在 server_test.go 顶部 import 块补充：

```go
	"encoding/base64"

	"agentbattle/protocol"
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race -count=1 ./runner/internal/dryrun/ -run TestUpload`
Expected: FAIL——`TestUploadOrchestration` 中 004_upload_a.json 不存在（骨架把两侧都落成 `NNN_upload.json` 无分侧、无 events 附加，r2 拿不到 done）。

- [ ] **Step 3: 实现完整 handleResult**

在 `runner/internal/dryrun/server.go` 中，把骨架版 handleResult 整体替换为：

```go
// resultReq 镜像 client.resultReq 的请求体形态（internal 边界，本地最小解析）。
type resultReq struct {
	Side           string `json:"side"`
	Passed         int    `json:"passed"`
	Total          int    `json:"total"`
	WallMS         int64  `json:"wall_ms"`
	DiffHash       string `json:"diff_hash,omitempty"`
	EventsGZBase64 string `json:"events_gz_base64,omitempty"`
}

// handleResult 编排 POST /api/matches/{id}/results：首侧上报应答 waiting，
// 双侧到齐按通过率判 winner（同则 tie——影子赛只演示数据，不预测平台的
// 计时规则）。事件流解码成功附加进导出文件，失败记 events_decode_error
// 降级不中断（与复盘管道降级口径一致）。
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	matchID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || matchID <= 0 {
		http.Error(w, "match id 非数字", http.StatusNotFound)
		return
	}
	bs, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req resultReq
	_ = json.Unmarshal(bs, &req)

	df := newDumpFile(r, bs)
	if req.EventsGZBase64 != "" {
		if evs, derr := decodeEvents(req.EventsGZBase64); derr != nil {
			df.EventsDecodeError = derr.Error()
		} else {
			df.Events = evs
		}
	}
	note := fmt.Sprintf("side=%s passed=%d/%d", req.Side, req.Passed, req.Total)
	if df.EventsDecodeError != "" {
		note += " 事件流不可解码"
	} else {
		note += fmt.Sprintf(" events=%d条", len(df.Events))
	}

	s.mu.Lock()
	if s.sides[matchID] == nil {
		s.sides[matchID] = map[string]resultSide{}
	}
	s.sides[matchID][req.Side] = resultSide{Passed: req.Passed, Total: req.Total}
	s.dumpFileLocked(r, df, "upload"+sideSuffix(req.Side), note)
	var resp map[string]any
	if len(s.sides[matchID]) < 2 {
		resp = map[string]any{"status": "waiting"}
	} else {
		resp = map[string]any{
			"status":   "done",
			"winner":   judgeWinner(s.sides[matchID]["a"], s.sides[matchID]["b"]),
			"rating_a": 1200.0,
			"rating_b": 1200.0,
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// judgeWinner 按通过比例判：高者胜，同则 tie（与规格口径一致；
// 不复刻 session.winner 的 WallMS 破平——影子赛不预测平台计时规则）。
func judgeWinner(a, b resultSide) string {
	sa, sb := ratio(a), ratio(b)
	switch {
	case sa > sb:
		return "a"
	case sb > sa:
		return "b"
	}
	return "tie"
}

func ratio(s resultSide) float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Passed) / float64(s.Total)
}
```

- [ ] **Step 4: 运行测试转绿**

Run: `go build ./... && go vet ./... && go test -race -count=1 ./runner/internal/dryrun/`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add runner/internal/dryrun/server.go runner/internal/dryrun/server_test.go
git commit -m "$(cat <<'EOF'
feat: dryrun upload 编排——waiting/done 双阶段、通过率判 winner、事件流解码附加

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: dryrun 对抗加固测试——404 / 10MB 帽 / 并发序号 / dump IO 失败

**Files:**
- Test: `runner/internal/dryrun/server_test.go`（末尾追加，无实现改动——本任务验证既有防御并暴露缺口，若跑红则修 server.go）

- [ ] **Step 1: 追加对抗用例**

```go
// TestUnknownRoute404 白名单外路由 fail-fast：GET 天梯、畸形 match id。
func TestUnknownRoute404(t *testing.T) {
	s, _ := newTestServer(t, "A", "B")
	resp, err := http.Get(s.Base() + "/api/ladder")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("非白名单路由应 404: %d", resp.StatusCode)
	}
	r2 := upload(t, s.Base(), 0, `{"side":"a"}`) // match id 0 → 404
	if r2.StatusCode != http.StatusNotFound {
		t.Fatalf("match id 0 应 404: %d", r2.StatusCode)
	}
}

// TestOversizedBody413 超 10MB 帽：413 且不落盘。
func TestOversizedBody413(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	big := make([]byte, maxBody+1)
	resp, err := http.Post(s.Base()+"/api/agents", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超帽应 413: %d", resp.StatusCode)
	}
	ents, err := os.ReadDir(filepath.Join(out, "dry_run"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("触帽请求不应落盘: %d 个文件", len(ents))
	}
}

// TestConcurrentRequests 并发上报：文件序号不重不串、响应 id 唯一。
func TestConcurrentRequests(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	const n = 20
	var wg sync.WaitGroup
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(s.Base()+"/api/agents", "application/json",
				strings.NewReader(fmt.Sprintf(`{"name":"n%d"}`, i)))
			if err != nil {
				t.Errorf("并发请求失败: %v", err)
				return
			}
			var out map[string]any
			json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			ids[i] = int64(out["id"].(float64))
		}(i)
	}
	wg.Wait()
	ents, err := os.ReadDir(filepath.Join(out, "dry_run"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != n {
		t.Fatalf("应落盘 %d 个文件: %d", n, len(ents))
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id == 0 || seen[id] {
			t.Fatalf("响应 id 应唯一且非零: %v", ids)
		}
		seen[id] = true
	}
	if len(s.Records()) != n {
		t.Fatalf("Records 应有 %d 条: %d", n, len(s.Records()))
	}
}

// TestDumpIOFailure 启动后删除导出目录：写盘失败进 DumpError 不 panic。
func TestDumpIOFailure(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	if err := os.RemoveAll(filepath.Join(out, "dry_run")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.New(s.Base()).Register("A"); err != nil {
		t.Fatalf("写盘失败不应影响 HTTP 应答: %v", err)
	}
	if s.DumpError() == nil {
		t.Fatal("目录被删后写盘应记入 DumpError")
	}
}
```

- [ ] **Step 2: 运行（含全包回归）**

Run: `go build ./... && go vet ./... && go test -race -count=1 ./runner/internal/dryrun/`
Expected: 全部 PASS。若某条红（如并发下有竞态、触帽未拦截），定位修复 server.go 后重跑至绿——本任务的价值就是让对抗用例逼出防御缺口。

- [ ] **Step 3: Commit**

```bash
git add runner/internal/dryrun/server.go runner/internal/dryrun/server_test.go
git commit -m "$(cat <<'EOF'
test: dryrun 对抗加固——404 fail-fast、10MB 帽、并发序号、dump IO 失败

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: cmdMirror 接线——--dry-run 旗标 + CLI 单测

**Files:**
- Modify: `runner/cmd/agentbattle/mirror.go`（旗标注册、互斥校验、dry-run 分支、收尾输出）
- Modify: `runner/cmd/agentbattle/main.go`（usage 文案）
- Test: `runner/cmd/agentbattle/mirror_test.go`（新建）

- [ ] **Step 1: 写失败测试**

创建 `runner/cmd/agentbattle/mirror_test.go`：

```go
// mirror_test.go —— mirror 子命令 --dry-run 的负路径测试。
package main

import (
	"strings"
	"testing"
)

// TestMirrorDryRunNegative 互斥校验与必填校验；离线流程可达（任务预检拦截）。
func TestMirrorDryRunNegative(t *testing.T) {
	// --dry-run 与 --server 互斥
	err := cmdMirror([]string{"--task", "x", "--dry-run", "--server", "http://y"})
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("--dry-run 与 --server 应互斥: %v", err)
	}
	// 缺 --name-a/--name-b
	err = cmdMirror([]string{"--task", "x", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "--name-a") {
		t.Fatalf("--dry-run 缺名应报错: %v", err)
	}
	// 校验通过后进入既有流程（任务目录不存在 → 任务预检失败），
	// 证明 dry-run 不需要 --server 即可启动离线对战
	err = cmdMirror([]string{"--task", "does-not-exist", "--dry-run",
		"--name-a", "A", "--name-b", "B"})
	if err == nil || !strings.Contains(err.Error(), "任务预检失败") {
		t.Fatalf("离线流程应可达并被任务预检拦截: %v", err)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test -race ./runner/cmd/agentbattle/ -run TestMirrorDryRunNegative`
Expected: FAIL（`--dry-run` 旗标不存在，被 newFlagSet 当未知参数报错，错误文案不含"互斥"）。

- [ ] **Step 3: 实现 cmdMirror 接线**

`runner/cmd/agentbattle/mirror.go` 四处修改：

1. import 块加 `"agentbattle/runner/internal/dryrun"`。

2. 旗标注册（`fixB := fs.String(...)` 行之后、envA/envB 之前）加：

```go
	dryRun := fs.Bool("dry-run", false, "影子赛：流程照常但请求发往本地假服务端并导出全部 payload（与 --server 互斥）")
```

3. 互斥校验（`if *task == ""` 校验之前）加：

```go
	// 影子赛与联网互斥：dry-run 的承诺就是零出网，给了 --server 只会让
	// 用户误以为数据发去了平台
	if *dryRun && *server != "" {
		return fmt.Errorf("--dry-run 与 --server 互斥（影子赛零出网）")
	}
```

4. 联网分支之后（`cl = client.New(*server)` 那个 if 块结束后）加 dry-run 分支，并把 Report 回调与收尾改为用局部 token 变量。将原 50-61 行的 `var cl *client.Client; tid := *taskID; if *server != "" {...}` 块替换为：

```go
	// 联网上报模式参数校验：缺一即拦在开赛前，避免跑到一半才发现没法上报
	var cl *client.Client
	var ds *dryrun.Server
	tid := *taskID
	if *server != "" {
		if *nameA == "" || *tokA == "" || *nameB == "" || *tokB == "" {
			return fmt.Errorf("--server 模式需要 --name-a/--token-a/--name-b/--token-b")
		}
		if tid == "" {
			tid = filepath.Base(*task)
		}
		cl = client.New(*server)
	}
	// 影子赛：本地起假服务端，代发两次注册取占位 token，client 全部
	// 流量指向 127.0.0.1——mirror 主流程零改动
	if *dryRun {
		if *nameA == "" || *nameB == "" {
			return fmt.Errorf("--dry-run 模式需要 --name-a/--name-b（将作为注册 payload 的真实字段）")
		}
		if tid == "" {
			tid = filepath.Base(*task)
		}
	}
```

然后 `outDir` 赋值（`mustOutDir`）之后、`cfg := session.MirrorConfig{...}` 之前插入：

```go
	tokA2, tokB2 := *tokA, *tokB
	if *dryRun {
		d, err := dryrun.NewServer(dryrun.Options{OutDir: outDir, NameA: *nameA, NameB: *nameB})
		if err != nil {
			return err
		}
		defer d.Close()
		ds = d
		dcl := client.New(d.Base())
		if _, tok, err := dcl.Register(*nameA); err != nil {
			return fmt.Errorf("dry-run 注册 %s 失败: %w", *nameA, err)
		} else {
			tokA2 = tok
		}
		if _, tok, err := dcl.Register(*nameB); err != nil {
			return fmt.Errorf("dry-run 注册 %s 失败: %w", *nameB, err)
		} else {
			tokB2 = tok
		}
		cl = dcl
	}
```

再把 Report 回调（原 `if cl != nil { cfg.Report = ... reportRound(os.Stdout, cl, r, *tokA, *tokB, ...) }`）改为用 tokA2/tokB2：

```go
	if cl != nil {
		cfg.Report = func(_ context.Context, r int, resA, resB session.Result, errA, errB error) error {
			return reportRound(os.Stdout, cl, r, tokA2, tokB2, tid, *nameA, *nameB, resA, resB, errA, errB)
		}
	}
```

5. 收尾输出：在 `fmt.Printf("明细: %s\n", outDir)` 之后、`if cl != nil {` 天梯块之前插入 dry-run 摘要，并给天梯块加非 dry-run 条件。将这两段整体替换为：

```go
	// 影子赛：逐条打印拦截摘要 + 隐私对照声明；不拉天梯（平台→runner
	// 方向不属于"平台会收到的数据"）
	if *dryRun {
		for _, rec := range ds.Records() {
			fmt.Printf("  [%02d] %s %s → %s  %s\n", rec.Seq, rec.Method, rec.Path, rec.File, rec.Note)
		}
		if err := ds.DumpError(); err != nil {
			return fmt.Errorf("dry-run 导出失败: %w", err)
		}
		fmt.Printf("payload 已导出: %s\n", filepath.Join(outDir, "dry_run"))
		fmt.Println("影子赛完成：以上即平台将收到的全部数据。全程未出网；事件流仅含路径与操作类型，不含文件内容与用户配置（CLAUDE.md/skills/记忆永不上传）。")
		return nil
	}
	// 联网模式：全部轮次结束后拉取天梯前 5 名展示结算成果。
	// 拉取失败只警告不改退出码——此时全部轮次上报已成功、Elo 已入账。
	if cl != nil {
		fmt.Println("天梯前 5:")
		if err := runLadder(os.Stdout, *server, 5); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 拉取天梯失败（结算不受影响）: %v\n", err)
		}
	}
	return nil
```

- [ ] **Step 4: main.go usage 更新**

`runner/cmd/agentbattle/main.go` usage 字符串中 mirror 块（`agentbattle mirror   --task ...` 起的几行）末尾加一行：

```
                        [--dry-run 零出网影子赛，导出全部 payload]
```

（缩进对齐既有 usage 块；doc 注释第 3-4 行的子命令列举可顺带补"影子赛"字样，非必须。）

- [ ] **Step 5: 运行转绿**

Run: `go build ./... && go vet ./... && go test -race -count=1 ./runner/cmd/agentbattle/ ./runner/internal/dryrun/`
Expected: 全部 PASS。

- [ ] **Step 6: Commit**

```bash
git add runner/cmd/agentbattle/mirror.go runner/cmd/agentbattle/mirror_test.go runner/cmd/agentbattle/main.go
git commit -m "$(cat <<'EOF'
feat: mirror --dry-run 旗标——本地假服务端接线、payload 导出与隐私声明

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: E2E 验收 + 收官

**Files:**
- Modify: `runner/e2e/platform_e2e_test.go`（末尾追加 TestDryRunMirror）
- Modify: `CHANGE.md`、`CLAUDE.md`（收官记录）

- [ ] **Step 1: 写 E2E（追加到 platform_e2e_test.go 末尾）**

```go
// TestDryRunMirror M2 计划 3 验收：mirror --dry-run 零出网跑通 1 局，
// 五个导出 payload 齐全、内容正确、stdout 含隐私声明。不构 serverBin、
// 不起平台进程——影子赛本就不需要 --server。确定性来源与
// TestPlatformLoopEcho 相同：--fix-a 使 A 2/2、B 1/2。
func TestDryRunMirror(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过黑盒 E2E")
	}
	root := findRepoRoot(t)
	bin := filepath.Join(t.TempDir(), "agentbattle.exe")
	buildBin(t, root, bin, "./runner/cmd/agentbattle")

	out := filepath.Join(t.TempDir(), "reports")
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	rep := run("mirror", "--dry-run",
		"--task", filepath.Join(root, "examples", "fix-add"),
		"--task-id", "fix-add",
		"--agent", "echo",
		"--fix-a", "add() { echo $(( $1 + $2 )); }",
		"--rounds", "1",
		"--name-a", "dryA", "--name-b", "dryB",
		"--out", out)

	for _, want := range []string{
		"镜像对战完成", "影子赛完成", "全程未出网",
		"001_register_a.json", "003_create_match.json", "005_upload_b.json",
		"winner=a",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("dry-run 输出缺 %q:\n%s", want, rep)
		}
	}

	// 五个 payload 齐全且关键字段正确
	dryDir := filepath.Join(out, "dry_run")
	ents, err := os.ReadDir(dryDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 5 {
		t.Fatalf("应导出 5 个 payload: %d", len(ents))
	}
	assertContains := func(name string, subs ...string) {
		t.Helper()
		bs, err := os.ReadFile(filepath.Join(dryDir, name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		for _, s := range subs {
			if !strings.Contains(string(bs), s) {
				t.Fatalf("%s 缺 %q:\n%s", name, s, bs)
			}
		}
	}
	assertContains("001_register_a.json", `"name": "dryA"`, "dry-run-placeholder")
	assertContains("002_register_b.json", `"name": "dryB"`)
	assertContains("003_create_match.json", `"task_id": "fix-add"`, `"agent_a": "dryA"`, `"agent_b": "dryB"`)
	assertContains("004_upload_a.json", `"side": "a"`, `"passed": 2`, `"total": 2`, `"events": [`)
	assertContains("005_upload_b.json", `"side": "b"`, `"passed": 1`)

	// 互斥负路径：--dry-run + --server 非零退出且文案含"互斥"
	c := exec.Command(bin, "mirror", "--dry-run", "--server", "http://127.0.0.1:1",
		"--task", filepath.Join(root, "examples", "fix-add"))
	if b, err := c.CombinedOutput(); err == nil || !strings.Contains(string(b), "互斥") {
		t.Fatalf("--dry-run 与 --server 应互斥: %v\n%s", err, b)
	}
}
```

- [ ] **Step 2: 运行 E2E**

Run: `go test -race ./runner/e2e/ -run 'TestDryRunMirror|TestPlatformLoopEcho' -v`
Expected: 两个测试 PASS（旧 E2E 回归确认 mirror 改动无破坏）。

- [ ] **Step 3: 全量验证**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: 全部 PASS。注：`TestRunCommandTimeout`（judge 包）有已知负载抖动（100ms 阈值，挂账中），若它偶发失败复跑该包确认抖动并汇报，不要改代码。

- [ ] **Step 4: 收官记录（先 Read 再 Edit，保持"最新在上"惯例）**

- `CHANGE.md` 条目区顶部（既有 M2 计划 2 条目之前）插入新条目：日期 2026-09-14、主题"M2 计划 3：--dry-run 影子赛"、核心变更点（`runner/internal/dryrun` 本地回环假服务端：三路由编排 + dump 原样落盘 + 事件流解码附加 / `mirror --dry-run` 旗标：与 `--server` 互斥、代发注册取占位 token、stdout 摘要与隐私声明 / 公开面防御：10MB 帽、白名单外 404、并发安全序号 / E2E TestDryRunMirror 零出网验收）、遗留事项（M2 三件交付物全部完成；dump IO 失败中止路径由单测覆盖；M3 Web Dashboard 可复用 dump 文件做"上传预览"页）。
- `CLAUDE.md` 的 CHANGE.md 简介行进度更新为：M1 与 M2 三件交付物（六维画像、复盘报告、--dry-run 影子赛）均已完成；M3 公测项（匹配系统、盲评、Web Dashboard）待启动。

- [ ] **Step 5: Commit**

```bash
git add runner/e2e/platform_e2e_test.go CHANGE.md CLAUDE.md
git commit -m "$(cat <<'EOF'
test: E2E 影子赛验收 TestDryRunMirror；M2 收官记录

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 6: 收官报告**

向控制器汇报：全部任务状态、偏离（如有）、全量验证证据。push 与合并 main 由控制器与用户确认后执行。

---

## Self-Review 记录

1. **规格覆盖**：§1 验证目标→T5 E2E；§2 五项决策（旗标入口→T4、回环拦截→T1/T4、文件+摘要交付→T1/T4、完全离线互斥→T4、占位凭证→T1）；§3 表格（register 代发→T4、create/upload 编排→T1/T2、dump 文件结构→T1、事件解码附加→T2）；§4 数据流→T4 接线；§5 组件（dryrun 包→T1/T2、mirror 小改→T4、红线→dump 全本地）；§6 错误处理（任务预检→T4 既有、解码降级→T2、IO 失败中止→T1 dumpErr+T4 收尾、404→T1/T3、互斥→T4）；§7 测试四层（对抗单测→T3、CLI 负路径→T4、E2E→T5）。
2. **占位符扫描**：T1 Step 1 的 TestDumpFidelity 首版含**故意标注**的 `noMux` 占位（先红教学设计，Step 2 给出最终版整体替换）；其余无 TBD/TODO。
3. **类型一致性**：`dryrun.Options/Server/Base/Close/Records/DumpError/Record/dumpFile`（T1 定义 → T3/T4/T5 消费）；`judgeWinner/resultSide/resultReq`（T2 定义 → T2 内部消费）；`tokA2/tokB2`（T4 局部变量，Report 回调闭包捕获）；`placeholder`/`maxBody`（T1 常量 → T2/T3 测试引用）一致。
4. **规格偏差已闭环**：bundle 路由死代码与 register 代发已在规格修订（ef0a3e1）中落档，计划与修订版规格一致。
