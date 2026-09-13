package client

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentbattle/protocol"
)

// zipEntry 是构造测试 zip 的辅助结构。
type zipEntry struct {
	Name string
	Data string
}

// buildZip 构造内存 zip 字节流。
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := w.Create(e.Name)
		if err != nil {
			t.Fatalf("构造 zip 条目 %s 失败: %v", e.Name, err)
		}
		if _, err := f.Write([]byte(e.Data)); err != nil {
			t.Fatalf("写入 zip 条目 %s 失败: %v", e.Name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 zip writer 失败: %v", err)
	}
	return buf.Bytes()
}

func TestRegisterAndMatchFlow(t *testing.T) {
	var gotXToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1,"name":"A","token":"tok-1"}`)
	})
	mux.HandleFunc("/api/matches", func(w http.ResponseWriter, r *http.Request) {
		gotXToken = r.Header.Get("X-Token")
		if gotXToken != "tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"未认证"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"match_id":7,"judge_key":"dev-secret"}`)
	})
	mux.HandleFunc("/api/matches/7/results", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"done","winner":"a","rating_a":1220,"rating_b":1180}`)
	})
	mux.HandleFunc("/api/ladder", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ladder":[{"name":"A","rating":1220,"games":2,"wins":1,"losses":1,"ties":0}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)

	id, token, err := c.Register("A")
	if err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	if id != 1 || token != "tok-1" {
		t.Fatalf("Register 返回错误: id=%d token=%q", id, token)
	}

	matchID, judgeKey, err := c.CreateMatch("tok-1", "fix-add", "A", "B")
	if err != nil {
		t.Fatalf("CreateMatch 失败: %v", err)
	}
	if matchID != 7 || judgeKey != "dev-secret" {
		t.Fatalf("CreateMatch 返回错误: matchID=%d judgeKey=%q", matchID, judgeKey)
	}
	if gotXToken != "tok-1" {
		t.Fatalf("/api/matches 请求未携带正确的 X-Token: %q", gotXToken)
	}

	settle, err := c.UploadResult("tok-1", 7, ResultIn{Side: "a", Passed: 2, Total: 2, WallMS: 100})
	if err != nil {
		t.Fatalf("UploadResult 失败: %v", err)
	}
	if settle.Status != "done" || settle.Winner != "a" || settle.RatingA != 1220 || settle.RatingB != 1180 {
		t.Fatalf("UploadResult 结算错误: %+v", settle)
	}

	rows, err := c.Ladder()
	if err != nil {
		t.Fatalf("Ladder 失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Ladder 应返回 1 行，实际 %d 行", len(rows))
	}
	if rows[0].Name != "A" || rows[0].Rating != 1220 || rows[0].Games != 2 || rows[0].Wins != 1 || rows[0].Losses != 1 || rows[0].Ties != 0 {
		t.Fatalf("Ladder 行内容错误: %+v", rows[0])
	}
}

func TestErrorSurfacing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"agent 名已存在: A"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	_, _, err := c.Register("A")
	if err == nil {
		t.Fatal("Register 应返回错误")
	}
	if !strings.Contains(err.Error(), "agent 名已存在") {
		t.Fatalf("错误信息未透出平台 body: %v", err)
	}
}

func TestLadderEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ladder", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ladder":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	rows, err := c.Ladder()
	if err != nil {
		t.Fatalf("Ladder 空列表不应报错: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("Ladder 应为空，实际 %d 行", len(rows))
	}
}

func TestExtractBundleRejectsTraversal(t *testing.T) {
	zipBytes := buildZip(t, []zipEntry{{Name: "../../evil.txt", Data: "evil"}})
	dest := t.TempDir()

	if err := ExtractBundle(zipBytes, dest); err == nil {
		t.Fatal("路径穿越条目应被拒绝")
	}

	// 断言目标目录及其父目录都没有 evil.txt
	checks := []string{
		filepath.Join(dest, "evil.txt"),
		filepath.Join(filepath.Dir(dest), "evil.txt"),
		filepath.Join(filepath.Dir(filepath.Dir(dest)), "evil.txt"),
	}
	for _, p := range checks {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("恶意文件被写出: %s", p)
		}
	}
}

func TestExtractBundleRejectsAbsPath(t *testing.T) {
	zipBytes := buildZip(t, []zipEntry{{Name: "/etc/evil", Data: "evil"}})
	dest := t.TempDir()

	if err := ExtractBundle(zipBytes, dest); err == nil {
		t.Fatal("绝对路径条目应被拒绝")
	}
}

func TestExtractBundleRoundTrip(t *testing.T) {
	entries := []zipEntry{
		{Name: "task.json", Data: `{"id":"fix-add"}`},
		{Name: "seed/calc.sh", Data: "#!/bin/sh\necho 42\n"},
		{Name: "tests/manifest.json", Data: `{"cases":[]}`},
		{Name: "tests/sig", Data: "deadbeef"},
		{Name: "a..b.txt", Data: "合法名，含 .. 字面量但非穿越"},
	}
	zipBytes := buildZip(t, entries)
	dest := t.TempDir()

	if err := ExtractBundle(zipBytes, dest); err != nil {
		t.Fatalf("正常解压失败: %v", err)
	}

	for _, e := range entries {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(e.Name)))
		if err != nil {
			t.Fatalf("解压后读取 %s 失败: %v", e.Name, err)
		}
		if string(got) != e.Data {
			t.Fatalf("%s 内容不一致: got %q want %q", e.Name, got, e.Data)
		}
	}
}

func TestGzipEvents(t *testing.T) {
	events := []protocol.Event{
		{Seq: 1, TS: 1000, Type: protocol.EventToolCall, Hash: "h1"},
		{Seq: 2, TS: 2000, Type: protocol.EventResult, Hash: "h2"},
	}
	gz, err := GzipEvents(events)
	if err != nil {
		t.Fatalf("GzipEvents 失败: %v", err)
	}
	if len(gz) == 0 {
		t.Fatal("GzipEvents 输出为空")
	}
	if strings.Contains(string(gz), "tool_call") {
		t.Fatal("输出不应为明文 NDJSON")
	}
}

// TestGzipEventsRoundtrip 验证 gzip 输出 gunzip 后是合法 NDJSON，
// 逐行 Unmarshal 回 []protocol.Event 与输入逐字段相等。
func TestGzipEventsRoundtrip(t *testing.T) {
	events := []protocol.Event{
		{Seq: 1, TS: 1000, Type: protocol.EventToolCall, Tool: "edit_file", ArgsHash: "ah1", DurationMS: 12, PrevHash: "", Hash: "h1"},
		{Seq: 2, TS: 2000, Type: protocol.EventResult, Tokens: 42, Note: "done", PrevHash: "h1", Hash: "h2"},
		{Seq: 3, TS: 3000, Type: protocol.EventFileEdit, Hash: "h3"},
	}
	gz, err := GzipEvents(events)
	if err != nil {
		t.Fatalf("GzipEvents 失败: %v", err)
	}

	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("输出不是合法 gzip: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip 失败: %v", err)
	}

	var got []protocol.Event
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var e protocol.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v (line=%q)", i+1, err, line)
		}
		got = append(got, e)
	}
	if len(got) != len(events) {
		t.Fatalf("roundtrip 事件数不符: got %d want %d", len(got), len(events))
	}
	if !reflect.DeepEqual(got, events) {
		t.Fatalf("roundtrip 事件不一致:\ngot  %+v\nwant %+v", got, events)
	}
}

// TestCreateMatchRequestBody 校验 /api/matches 请求体字段完整且值正确。
func TestCreateMatchRequestBody(t *testing.T) {
	var gotBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/matches", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"body 不是合法 JSON: %v"}`, err)
			return
		}
		if gotBody["task_id"] == "" || gotBody["agent_a"] == "" || gotBody["agent_b"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"缺少必填字段"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"match_id":7,"judge_key":"dev-secret"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	matchID, _, err := c.CreateMatch("tok", "fix-add", "alice", "bob")
	if err != nil {
		t.Fatalf("CreateMatch 失败: %v", err)
	}
	if matchID != 7 {
		t.Fatalf("matchID=%d, want 7", matchID)
	}
	want := map[string]string{"task_id": "fix-add", "agent_a": "alice", "agent_b": "bob"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("请求体字段错误:\ngot  %v\nwant %v", gotBody, want)
	}
}

// TestUploadResultRequestBody 校验 /api/matches/7/results 请求体字段，
// 以及 EventsGZ 经 base64 解码后是合法 gzip（gunzip 出合法 NDJSON）。
func TestUploadResultRequestBody(t *testing.T) {
	events := []protocol.Event{
		{Seq: 1, TS: 1000, Type: protocol.EventToolCall, Hash: "h1"},
		{Seq: 2, TS: 2000, Type: protocol.EventResult, PrevHash: "h1", Hash: "h2"},
	}
	eventsGZ, err := GzipEvents(events)
	if err != nil {
		t.Fatalf("构造 EventsGZ 失败: %v", err)
	}

	var gotBody map[string]any
	var gotRaw []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/api/matches/7/results", func(w http.ResponseWriter, r *http.Request) {
		bs, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotRaw = bs
		if err := json.Unmarshal(bs, &gotBody); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"body 不是合法 JSON: %v"}`, err)
			return
		}
		if gotBody["side"] != "a" || gotBody["passed"] != float64(2) || gotBody["total"] != float64(2) || gotBody["wall_ms"] != float64(100) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"字段值错误"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"done","winner":"a"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	settle, err := c.UploadResult("tok", 7, ResultIn{
		Side: "a", Passed: 2, Total: 2, WallMS: 100,
		DiffHash: "abc123", EventsGZ: eventsGZ,
	})
	if err != nil {
		t.Fatalf("UploadResult 失败: %v", err)
	}
	if settle.Status != "done" || settle.Winner != "a" {
		t.Fatalf("结算错误: %+v", settle)
	}

	// 字段值断言（Decode 后）
	if gotBody["side"] != "a" {
		t.Fatalf("side=%v, want \"a\"", gotBody["side"])
	}
	if gotBody["passed"] != float64(2) || gotBody["total"] != float64(2) {
		t.Fatalf("passed/total=%v/%v, want 2/2", gotBody["passed"], gotBody["total"])
	}
	if gotBody["wall_ms"] != float64(100) {
		t.Fatalf("wall_ms=%v, want 100", gotBody["wall_ms"])
	}
	if gotBody["diff_hash"] != "abc123" {
		t.Fatalf("diff_hash=%v, want \"abc123\"", gotBody["diff_hash"])
	}

	// events_gz_base64 → 解码 → 合法 gzip → 合法 NDJSON
	b64, _ := gotBody["events_gz_base64"].(string)
	if b64 == "" {
		t.Fatalf("请求体缺少 events_gz_base64: %s", gotRaw)
	}
	gzBytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("events_gz_base64 不是合法 base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		t.Fatalf("events_gz_base64 解码后不是合法 gzip: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip 失败: %v", err)
	}
	var gotEvents []protocol.Event
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var e protocol.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v (line=%q)", i+1, err, line)
		}
		gotEvents = append(gotEvents, e)
	}
	if !reflect.DeepEqual(gotEvents, events) {
		t.Fatalf("事件流 roundtrip 不一致:\ngot  %+v\nwant %+v", gotEvents, events)
	}
}

// TestUploadResultRequiresToken 校验 /api/matches/7/results 的 X-Token：
// 缺失或不符时 stub 返回 401，客户端应报错。
func TestUploadResultRequiresToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/matches/7/results", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "tok-right" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"未认证"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"done","winner":"a"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	in := ResultIn{Side: "a", Passed: 2, Total: 2, WallMS: 100}

	// 缺失 token（空串 → 客户端不带 X-Token 头）
	if _, err := c.UploadResult("", 7, in); err == nil {
		t.Fatal("缺失 X-Token 应返回错误")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应含 401: %v", err)
	}

	// token 不符
	if _, err := c.UploadResult("tok-wrong", 7, in); err == nil {
		t.Fatal("X-Token 不符应返回错误")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应含 401: %v", err)
	}

	// 正确 token 应成功
	if _, err := c.UploadResult("tok-right", 7, in); err != nil {
		t.Fatalf("正确 token 不应报错: %v", err)
	}
}

// TestExtractBundleRejectsOversizedEntry 构造一个 64MB+1 字节（全零，压缩后很小）
// 的条目，断言被 64MB 上限拒绝且目标目录没有写出该文件。
func TestExtractBundleRejectsOversizedEntry(t *testing.T) {
	oversized := bytes.Repeat([]byte{0}, (64<<20)+1)
	zipBytes := buildZip(t, []zipEntry{
		{Name: "normal.txt", Data: "ok"},
		{Name: "bomb.bin", Data: string(oversized)},
	})
	dest := t.TempDir()

	if err := ExtractBundle(zipBytes, dest); err == nil {
		t.Fatal("超过 64MB 的条目应被拒绝")
	} else if !strings.Contains(err.Error(), "64MB") {
		t.Fatalf("错误信息应说明 64MB 上限: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "bomb.bin")); err == nil {
		t.Fatal("超限文件不应被写出")
	}
	// 超限条目在 bomb.bin 之前成功写出，属预期（逐条目处理）；只断言恶意条目未落盘。
	if _, err := os.Stat(filepath.Join(dest, "normal.txt")); err != nil {
		t.Logf("提示: normal.txt 未写出（条目顺序在超限条目之前被中断）: %v", err)
	}
}

// TestExtractBundleCorruptZip 非法 zip 字节应返回错误而非 panic。
func TestExtractBundleCorruptZip(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractBundle([]byte("not a zip"), dest); err == nil {
		t.Fatal("损坏的 zip 应返回错误")
	}
}

// TestProfile 解析 /api/agents/{name}/profile 响应；路径转义防注入。
func TestProfile(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agent":"a/b","profiles":[`+
			`{"task_type":"general","sample_size":2,`+
			`"profile_json":"{\"dims\":{\"correctness\":{\"score\":75}}}",`+
			`"updated_at":123}]}`)
	}))
	defer srv.Close()
	profs, err := New(srv.URL).Profile("a/b")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/agents/a/b/profile" { // url.PathEscape 只转义 query 语义，Path 段内 / 保留
		t.Fatalf("路径错误: %s", gotPath)
	}
	if len(profs) != 1 || profs[0].TaskType != "general" || profs[0].SampleSize != 2 {
		t.Fatalf("解析错误: %+v", profs)
	}
	if !strings.Contains(profs[0].ProfileJSON, "correctness") {
		t.Fatalf("profile_json 应原样透传: %q", profs[0].ProfileJSON)
	}
}
