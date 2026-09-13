// profile_test.go —— CLI profile 子命令的输出格式测试。
package main

import (
	"bytes"
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

	buf.Reset()
	printProfile(&buf, "nobody", "general", profileDoc{})
	if !strings.Contains(buf.String(), "暂无") {
		t.Fatalf("零样本应打印提示:\n%s", buf.String())
	}
}
