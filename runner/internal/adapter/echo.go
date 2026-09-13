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
	Fail       bool   // true 时返回 error，模拟 agent 崩溃
	TargetFile string // 写入 FixContent 的目标文件名，空则默认 "calc.sh"
}

func (e Echo) Name() string { return "echo" }

func (e Echo) Detect() error { return nil }

func (e Echo) Launch(ctx context.Context, cwd, task string, env []string, out chan<- RawEvent) error {
	if e.Fail {
		return fmt.Errorf("echo: simulated crash")
	}
	target := e.TargetFile
	if target == "" {
		target = "calc.sh"
	}
	send := func(ev RawEvent) error {
		select {
		case out <- ev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	sum := sha256.Sum256([]byte(task))
	if err := send(RawEvent{Type: "tool_call", Tool: "Read", ArgsHash: hex.EncodeToString(sum[:])}); err != nil {
		return err
	}
	if err := send(RawEvent{Type: "tool_call", Tool: "Edit", ArgsHash: hex.EncodeToString(sum[:4]),
		Note: filepath.Join(cwd, target)}); err != nil {
		return err
	}
	if e.FixContent != "" {
		if err := os.WriteFile(filepath.Join(cwd, target), []byte(e.FixContent), 0o644); err != nil {
			return err
		}
		if err := send(RawEvent{Type: "file_edit", Tool: "Edit", Note: target}); err != nil {
			return err
		}
	}
	select {
	case <-time.After(10 * time.Millisecond): // 模拟耗时
	case <-ctx.Done():
		return ctx.Err()
	}
	return send(RawEvent{Type: "result", DurationMS: 10, Tokens: 42})
}
