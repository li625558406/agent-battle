// server_test.go —— dryrun 假服务端：编排正确性、dump 保真与对抗用例。
package dryrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
