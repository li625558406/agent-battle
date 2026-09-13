// profile_test.go —— CLI profile 子命令的输出格式测试。
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPrintProfile 六维固定顺序输出 + 低样本提示；空画像打印引导文案。
func TestPrintProfile(t *testing.T) {
	var buf bytes.Buffer
	doc := profileDoc{
		SampleSize: 2,
		LowSample:  true,
		Dims: map[string]profileDim{
			"correctness": {Score: 75, Raw: 1, Sample: 2},
			"stability":   {Score: 62.5, Raw: 0, Sample: 2},
		},
	}
	printProfile(&buf, "demoA", "general", doc)
	out := buf.String()
	for _, want := range []string{"demoA", "general", "样本不足", "correctness", "75.0", "stability", "62.5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺 %q:\n%s", want, out)
		}
	}
	// 六维顺序：correctness 在 tool_efficiency 前
	if strings.Index(out, "correctness") > strings.Index(out, "tool_efficiency") {
		t.Fatalf("维度顺序错误:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Fatalf("缺维应打印 — 占位:\n%s", out)
	}

	buf.Reset()
	printProfile(&buf, "nobody", "general", profileDoc{})
	if !strings.Contains(buf.String(), "暂无") {
		t.Fatalf("零样本应打印提示:\n%s", buf.String())
	}
}

// TestCmdProfileNegative 负路径：必填校验 / 空列表 / 畸形 profile_json / 缺维占位。
func TestCmdProfileNegative(t *testing.T) {
	// --server 缺失 / --name 缺失
	if err := cmdProfile([]string{}); err == nil {
		t.Fatal("--server 缺失应报错")
	}
	if err := cmdProfile([]string{"--server", "http://x"}); err == nil {
		t.Fatal("--name 缺失应报错")
	}

	// 空画像列表：打印"暂无"且不报错
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agent":"nobody","profiles":[]}`)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	if err := runProfile(&buf, srv.URL, "nobody"); err != nil {
		t.Fatalf("空列表不应报错: %v", err)
	}
	if !strings.Contains(buf.String(), "暂无") {
		t.Fatalf("空列表应打印暂无:\n%s", buf.String())
	}

	// 畸形 profile_json：报错且含 task_type
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agent":"x","profiles":[{"task_type":"general","sample_size":1,"profile_json":"{oops","updated_at":1}]}`)
	}))
	defer bad.Close()
	buf.Reset()
	if err := runProfile(&buf, bad.URL, "x"); err == nil {
		t.Fatal("畸形 profile_json 应报错")
	} else if !strings.Contains(err.Error(), "general") {
		t.Fatalf("错误信息应含 task_type: %v", err)
	}
}
