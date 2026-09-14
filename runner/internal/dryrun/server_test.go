// server_test.go —— dryrun 假服务端：编排正确性、dump 保真与对抗用例。
package dryrun

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"agentbattle/protocol"

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

// TestNewServerNonEmptyDir dry_run 目录已存在且非空时拒绝启动，防静默覆盖
// 上一次的审计产物（攻击用例：重跑同 --out）。
func TestNewServerNonEmptyDir(t *testing.T) {
	out := t.TempDir()
	dir := filepath.Join(out, "dry_run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "001_old.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewServer(Options{OutDir: out, NameA: "A", NameB: "B"})
	if err == nil || !strings.Contains(err.Error(), "非空") {
		t.Fatalf("dry_run 目录非空应报错: %v", err)
	}
}

// TestHandleResultBadID 攻击用例：非数字 match id 与 int64 溢出 id 均 404。
// （waiting/done 编排正路已由 TestUploadOrchestration 覆盖，此处只锁 id 校验。）
func TestHandleResultBadID(t *testing.T) {
	s, _ := newTestServer(t, "A", "B")
	for _, p := range []string{"/api/matches/abc/results", "/api/matches/99999999999999999999/results"} {
		resp, err := http.Post(s.Base()+p, "application/json", strings.NewReader(`{"side":"a"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 应 404: %d", p, resp.StatusCode)
		}
	}
}

// TestDumpFailure 写盘失败：DumpError 记录、Record 仍登记且 note 保留原描述。
func TestDumpFailure(t *testing.T) {
	s, out := newTestServer(t, "echoA", "echoB")
	// 删除导出目录并替换为同名普通文件，迫使 WriteFile 失败
	dir := filepath.Join(out, "dry_run")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.New(s.Base()).Register("echoA"); err != nil {
		t.Fatalf("写盘失败不应影响 HTTP 应答: %v", err)
	}
	if s.DumpError() == nil {
		t.Fatal("写盘失败应记入 DumpError")
	}
	recs := s.Records()
	if len(recs) != 1 || !strings.Contains(recs[0].Note, "dump 失败") || !strings.Contains(recs[0].Note, "echoA") {
		t.Fatalf("失败 Record 应含 dump 失败标记且保留原 note: %+v", recs)
	}
}

// TestReadBodyErrorClassification 读 body 错误分类：仅超 10MB 帽回 413，
// 其余读错误（断连等）回 400，不再一律 413。
func TestReadBodyErrorClassification(t *testing.T) {
	// 超帽 → 413
	s, _ := newTestServer(t, "A", "B")
	big := strings.Repeat("a", maxBody+1)
	resp, err := http.Post(s.Base()+"/api/agents", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超帽应 413: %d", resp.StatusCode)
	}
	// 非超帽读错误（底层 reader 故障）→ 400 而非 413
	errBroken := errors.New("模拟断连")
	req := httptest.NewRequest(http.MethodPost, "/api/agents", io.NopCloser(iotest.ErrReader(errBroken)))
	rec := httptest.NewRecorder()
	(&Server{}).readBody(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非超帽读错误应 400: %d", rec.Code)
	}
}

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
// 全新目录 seq 从 1 起：本测共 4 次 upload → 001/002/003/004。
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
	dfa := readDump(t, out, "001_upload_a.json")
	if len(dfa.Events) != 2 {
		t.Fatalf("upload_a 应附加 2 条事件: %d", len(dfa.Events))
	}
	if !bytes.Contains(dfa.Events[1], []byte(`"error"`)) {
		t.Fatalf("事件内容不符: %s", dfa.Events[1])
	}
	dfb := readDump(t, out, "002_upload_b.json")
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

// TestDumpBodyFidelityWithPlaceholderToken 攻击用例（挂账 1）：请求 body
// 与事件字符串值都含 body 占位符字面量——旧 LastIndex 定位会被事件内容
// 误导击穿保真；分段确定性拼接无任何搜索，body 必须逐字节无损。
func TestDumpBodyFidelityWithPlaceholderToken(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	gz, err := client.GzipEvents([]protocol.Event{
		{Seq: 1, TS: 1, Type: protocol.EventToolCall, Tool: "@@DRYRUN_BODY@@", Hash: "h1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(gz)
	raw := fmt.Sprintf(`{"side":"a","passed":2,"total":2,"note":"@@DRYRUN_BODY@@","events_gz_base64":%q}`, b64)
	resp, err := http.Post(s.Base()+"/api/matches/1/results", "application/json", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// readDump 内部 json.Unmarshal 同时验证导出文件整体仍是合法 JSON
	df := readDump(t, out, "001_upload_a.json")
	if string(df.Body) != raw {
		t.Fatalf("含占位符字面量时 body 仍应逐字节保真:\nwant %s\ngot  %s", raw, df.Body)
	}
	if len(df.Events) != 1 || !bytes.Contains(df.Events[0], []byte("@@DRYRUN_BODY@@")) {
		t.Fatalf("事件应保留占位符字面量: %s", df.Events)
	}
}

// TestDecodeEventsZipBomb 攻击用例（挂账 2）：解压后超过 64MB 帽的 gzip 流
// 必须报错（zip 炸弹不打爆内存），且端到端记 events_decode_error 降级、
// HTTP 流程不中断。
func TestDecodeEventsZipBomb(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(bytes.Repeat([]byte{0}, (64<<20)+1)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	if _, err := decodeEvents(b64); err == nil {
		t.Fatal("解压后超 64MB 帽的事件流应报错")
	}

	// 端到端：超帽走降级路径，不中断编排
	s, out := newTestServer(t, "A", "B")
	r := decodeResp(t, upload(t, s.Base(), 1,
		fmt.Sprintf(`{"side":"a","passed":0,"total":2,"events_gz_base64":%q}`, b64)))
	if r["status"] != "waiting" {
		t.Fatalf("超帽降级不应中断编排: %v", r)
	}
	df := readDump(t, out, "001_upload_a.json")
	if df.EventsDecodeError == "" {
		t.Fatalf("超帽应记 events_decode_error: %+v", df)
	}
}

// TestUploadUnknownSideNotDone 攻击用例：side:"c" 等杂侧只落盘不参与结算——
// 上报 a 后再上报 c 不得凭 len==2 用零值幻影 b 触发 done；a/b 到齐才结算。
func TestUploadUnknownSideNotDone(t *testing.T) {
	s, out := newTestServer(t, "A", "B")
	r1 := decodeResp(t, upload(t, s.Base(), 1, `{"side":"a","passed":1,"total":2}`))
	if r1["status"] != "waiting" {
		t.Fatalf("首侧应 waiting: %v", r1)
	}
	// 杂侧 c 通过率 2/2 高于 a——若被误当 b 结算，winner 会是幻影侧
	r2 := decodeResp(t, upload(t, s.Base(), 1, `{"side":"c","passed":2,"total":2}`))
	if r2["status"] != "waiting" {
		t.Fatalf("side=c 不得触发 done（零值幻影 b）: %v", r2)
	}
	readDump(t, out, "002_upload.json") // 杂侧仍落盘，无后缀
	r3 := decodeResp(t, upload(t, s.Base(), 1, `{"side":"b","passed":1,"total":2}`))
	if r3["status"] != "done" || r3["winner"] != "tie" {
		t.Fatalf("a/b 到齐才 done，且 c 的 2/2 不应计入: %v", r3)
	}
}

// TestMarshalDumpFileEventsWithDecodeError events 与 events_decode_error
// 同时存在（handleResult 中互斥、不可达的组合）时，marshalDumpFile 分段
// 拼接仍必须是合法 JSON 且两字段都在——直接构造结构锁定序列化器。
func TestMarshalDumpFileEventsWithDecodeError(t *testing.T) {
	df := &dumpFile{
		Method: "POST", Path: "/api/matches/1/results",
		Headers:           map[string]string{"X-T": "v"},
		Body:              json.RawMessage(`{"side":"a"}`),
		Events:            []json.RawMessage{json.RawMessage(`{"seq":1}`)},
		EventsDecodeError: "事件流 NDJSON 解析失败: boom",
	}
	bs, err := marshalDumpFile(df)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(bs) {
		t.Fatalf("两字段共存时拼接应为合法 JSON:\n%s", bs)
	}
	var back dumpFile
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, bs)
	}
	// events 走 MarshalIndent 重缩进（语义等价），不能按原始字节比对
	if len(back.Events) != 1 || !bytes.Contains(back.Events[0], []byte(`"seq"`)) {
		t.Fatalf("events 丢失/不符: %s", back.Events)
	}
	if back.EventsDecodeError != df.EventsDecodeError {
		t.Fatalf("events_decode_error 丢失: %q", back.EventsDecodeError)
	}
}

// TestDecodeEventsExactCapBoundary 恰好等于 64MB 解压帽的流必须通过
//（帽 +1 边界不变量：只有真实解压量超过帽才报错）。用一个合法事件行 +
// 换行白填充到恰好 maxDecompressed 字节。
func TestDecodeEventsExactCapBoundary(t *testing.T) {
	var payload bytes.Buffer
	payload.WriteString(`{"seq":1,"type":"tool_call","tool":"bash"}`)
	payload.WriteByte('\n')
	for payload.Len() < maxDecompressed {
		payload.WriteByte('\n') // NDJSON 之后的纯空白，合法且不产生事件
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if payload.Len() != maxDecompressed {
		t.Fatalf("前置校验：payload 应恰好 %d 字节，实际 %d", maxDecompressed, payload.Len())
	}
	evs, err := decodeEvents(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("恰好等于帽的流应通过: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("应恰好解码出 1 条事件: %d", len(evs))
	}
}

// TestDecodeEventsMalformedNDJSON gzip 合法但解压内容非 JSON → 应报
// "NDJSON 解析失败"路径，而非 gzip/base64 错误。
func TestDecodeEventsMalformedNDJSON(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := decodeEvents(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "NDJSON 解析失败") {
		t.Fatalf("应报 NDJSON 解析失败: %v", err)
	}
}
