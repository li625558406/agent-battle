// Package dryrun 实现 --dry-run 影子赛的本地回环假服务端：把 mirror 流程
// 本应发给平台的请求原样落盘，并应答预编排的最小合法响应，让用户在零出网
// 前提下逐字节核查平台将收到的数据（隐私承诺，设计文档 §7.3）。
package dryrun

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// dumpFile 导出文件结构：平台将收到的请求方法、路径、请求头与 body
// （Host/Content-Length 由协议层剥离，不在 headers 内）。
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
	// 重跑防护：目录非空说明上次导出残留，静默覆盖会毁掉审计产物，直接拒绝；
	// ReadDir 本身出错（目录可读性异常）同样报错，不 fail-open 放行
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("检查 dry_run 导出目录失败: %w", err)
	}
	if len(ents) > 0 {
		return nil, fmt.Errorf("dry_run 目录非空（%d 个文件），疑似重跑——请换 --out 或清理后重试", len(ents))
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
// 降级不中断（与复盘管道降级口径一致）。sides 写入与 dump 落盘同持 mu，
// 保证导出文件序与编排状态一致。
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
	_ = json.Unmarshal(bs, &req) // 畸形 body → 全零值，仍如实落盘

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

// ---- dump 落盘 ----

// readBody 读请求体（10MB 帽）：仅超帽回 413 不落盘；其余读错误
// （断连等）回 400，不与超帽混报。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	bs, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "请求体超过 10MB 帽", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "读取请求体失败", http.StatusBadRequest)
		}
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

// marshalDumpFile 序列化导出文件：分段确定性拼接——method/path/headers
// 前导段走 MarshalIndent 便于人读，body/events 等字段由代码手工续写，
// 全程无任何字符串搜索/替换（不出现占位符），彻底消除"body 或事件内容
// 恰好含某标记字面量被误定位"的碰撞类。body 以原始字节嵌入，逐字节保真
// （encoding/json 会对 json.RawMessage 做压缩/重缩进，直接序列化做不到）；
// events 为语义等价的重缩进（人读优先，内容无损）。
func marshalDumpFile(df *dumpFile) ([]byte, error) {
	head, err := json.MarshalIndent(struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	}{Method: df.Method, Path: df.Path, Headers: df.Headers}, "", "  ")
	if err != nil {
		return nil, err
	}
	// MarshalIndent 输出恒以 '}' 收尾——确定性切片去壳续写，非内容搜索
	head = head[:len(head)-1]

	out := make([]byte, 0, len(head)+len(df.Body)+256)
	out = append(out, head...)
	out = append(out, ",\n  \"body\": "...)
	out = append(out, df.Body...)

	if len(df.Events) > 0 {
		evs, err := json.MarshalIndent(df.Events, "  ", "  ")
		if err != nil {
			return nil, fmt.Errorf("序列化 events 失败: %w", err)
		}
		out = append(out, ",\n  \"events\": "...)
		out = append(out, evs...)
	}
	if df.EventsDecodeError != "" {
		q, err := json.Marshal(df.EventsDecodeError)
		if err != nil {
			return nil, fmt.Errorf("序列化 events_decode_error 失败: %w", err)
		}
		out = append(out, ",\n  \"events_decode_error\": "...)
		out = append(out, q...)
	}
	return append(out, '\n', '}'), nil
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
		if note != "" {
			note = "dump 失败(" + note + "): " + err.Error()
		} else {
			note = "dump 失败: " + err.Error()
		}
	}
	s.records = append(s.records, Record{
		Seq: s.seq, Method: r.Method, Path: r.URL.Path, File: name, Note: note,
	})
}

// newDumpFile 组装导出结构：headers 展平；body 为合法 JSON（含前导空白
// 的合法 JSON——json.Valid 容忍前导空白，不做任何裁剪，原样嵌入内容无损）
// 时逐字节保真嵌入；非法 JSON 字符串化记录（转义无损还原原文，且保证
// 导出文件永远是合法 JSON）。
func newDumpFile(r *http.Request, body []byte) *dumpFile {
	hdrs := map[string]string{}
	for k, v := range r.Header {
		hdrs[k] = strings.Join(v, ",")
	}
	if len(body) > 0 && (body[0] == '{' || body[0] == '[') && json.Valid(body) {
		return &dumpFile{Method: r.Method, Path: r.URL.Path,
			Headers: hdrs, Body: json.RawMessage(body)}
	}
	q, err := json.Marshal(string(body))
	if err != nil {
		q = []byte(`"<marshal fallback failed>"`)
	}
	return &dumpFile{Method: r.Method, Path: r.URL.Path,
		Headers: hdrs, Body: json.RawMessage(q)}
}

// maxDecompressed 单次事件流解压后的总量帽（64MB，与 client.ExtractBundle
// 单文件帽对齐）：gzip 压缩比可达千倍，无帽的 zip 炸弹会打爆内存。
const maxDecompressed = 64 << 20

// decodeEvents 解码 events_gz_base64（base64 → gunzip → NDJSON）为事件数组；
// 任一步失败返回错误（handleResult 记 events_decode_error 降级消费）。
// 解压经总量帽防护，超帽报错而非静默截断。
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
	// 帽 +1：只有真实解压量超过帽才触界，恰好等于帽的合法流不受影响
	bs, err := io.ReadAll(io.LimitReader(zr, maxDecompressed+1))
	if err != nil {
		return nil, fmt.Errorf("事件流解压读取失败: %w", err)
	}
	if int64(len(bs)) > maxDecompressed {
		return nil, fmt.Errorf("事件流解压后超过 %dMB 上限（疑似 zip 炸弹）", maxDecompressed>>20)
	}
	dec := json.NewDecoder(bytes.NewReader(bs))
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
