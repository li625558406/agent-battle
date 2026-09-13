package client

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
