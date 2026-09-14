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

// resultSide 记录一侧上报的通过数（双侧到齐后判 winner 用，Task 2 消费）。
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

// bodyPlaceholder 占据序列化输出中 body 字段的位置，落盘前被替换回原始
// body 字节——encoding/json 会对 json.RawMessage 内容做压缩/重缩进，
// 直接序列化无法逐字节保真。
const bodyPlaceholder = "@@DRYRUN_BODY@@"

// marshalDumpFile 序列化导出结构：envelope 走 MarshalIndent 便于人读，
// body 以原始字节原样拼接（保真核心）。
func marshalDumpFile(df *dumpFile) ([]byte, error) {
	type envelope struct {
		Method            string            `json:"method"`
		Path              string            `json:"path"`
		Headers           map[string]string `json:"headers"`
		Body              string            `json:"body"`
		Events            []json.RawMessage `json:"events,omitempty"`
		EventsDecodeError string            `json:"events_decode_error,omitempty"`
	}
	b, err := json.MarshalIndent(envelope{
		Method: df.Method, Path: df.Path, Headers: df.Headers,
		Body: bodyPlaceholder, Events: df.Events, EventsDecodeError: df.EventsDecodeError,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	// 定位最后一次出现：编码器按字段序写 headers→body，若某 header 值恰好
	// 含占位符字符串，首次出现会在 headers 里，替换它将损坏导出文件。
	ph := []byte(`"` + bodyPlaceholder + `"`)
	i := bytes.LastIndex(b, ph)
	if i < 0 {
		return nil, fmt.Errorf("占位符丢失，无法拼接 body")
	}
	out := make([]byte, 0, len(b)-len(ph)+len(df.Body))
	out = append(out, b[:i]...)
	out = append(out, df.Body...)
	return append(out, b[i+len(ph):]...), nil
}

// dumpFileLocked 序号命名落盘并登记 Record；写盘失败记入 dumpErr（CLI 收尾中止）。
func (s *Server) dumpFileLocked(r *http.Request, df *dumpFile, slug, note string) {
	s.seq++
	name := fmt.Sprintf("%03d_%s.json", s.seq, slug)
	b, err := marshalDumpFile(df)
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
// 任一步失败返回错误（Task 2 的 handleResult 消费）。
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
