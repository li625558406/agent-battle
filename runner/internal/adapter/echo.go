package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Echo 是测试替身 agent：发几个假事件，写入 FixContent 作为"解法"，
// 正常返回。FixContent 为空则不写文件（用于制造判分失败样本）。
type Echo struct {
	FixContent string
	Fail       bool // true 时返回 error，模拟 agent 崩溃
}

func (e Echo) Name() string { return "echo" }

func (e Echo) Detect() error { return nil }

func (e Echo) Launch(ctx context.Context, cwd, task string, env []string, out chan<- RawEvent) error {
	if e.Fail {
		return fmt.Errorf("echo: simulated crash")
	}
	sum := sha256.Sum256([]byte(task))
	out <- RawEvent{Type: "tool_call", Tool: "Read", ArgsHash: hex.EncodeToString(sum[:])}
	out <- RawEvent{Type: "tool_call", Tool: "Edit", ArgsHash: hex.EncodeToString(sum[:4]),
		Note: filepath.Join(cwd, "calc.sh")}
	if e.FixContent != "" {
		if err := os.WriteFile(filepath.Join(cwd, "calc.sh"), []byte(e.FixContent), 0o644); err != nil {
			return err
		}
		out <- RawEvent{Type: "file_edit", Tool: "Edit", Note: "calc.sh"}
	}
	time.Sleep(10 * time.Millisecond) // 模拟耗时
	out <- RawEvent{Type: "result", DurationMS: 10, Tokens: 42}
	return nil
}
