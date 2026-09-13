package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEchoSolvesTask(t *testing.T) {
	cwd := t.TempDir()
	a := Echo{FixContent: "fixed"}
	ch := make(chan RawEvent, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := a.Launch(ctx, cwd, "fix it", nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	close(ch)
	types := map[string]int{}
	for e := range ch {
		types[e.Type]++
	}
	if types["tool_call"] == 0 || types["result"] == 0 {
		t.Fatalf("echo should emit tool_call and result, got %v", types)
	}
	if types["file_edit"] != 1 {
		t.Fatalf("file_edit should be emitted exactly once, got %d", types["file_edit"])
	}
	got, err := os.ReadFile(filepath.Join(cwd, "calc.sh"))
	if err != nil {
		t.Fatalf("fix file not written: %v", err)
	}
	if string(got) != "fixed" {
		t.Fatalf("fix file content mismatch: got %q, want %q", got, "fixed")
	}
}

// Fail 路径：返回 error，不写文件。
func TestEchoFail(t *testing.T) {
	cwd := t.TempDir()
	a := Echo{Fail: true}
	ch := make(chan RawEvent, 16)
	err := a.Launch(context.Background(), cwd, "fix it", nil, ch)
	if err == nil {
		t.Fatal("echo with Fail=true should return error")
	}
	if _, statErr := os.Stat(filepath.Join(cwd, "calc.sh")); !os.IsNotExist(statErr) {
		t.Fatal("echo with Fail=true must not write fix file")
	}
}

// I4：Launch 前 ctx 已取消，发送事件应立即返回 ctx.Err()。
func TestEchoRespectsCancelledContext(t *testing.T) {
	cwd := t.TempDir()
	a := Echo{FixContent: "fixed"}
	// 不排空的零缓冲 channel：保证发送阻塞，靠 select ctx.Done() 退出。
	ch := make(chan RawEvent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := a.Launch(ctx, cwd, "fix it", nil, ch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Launch should return context.Canceled, got %v", err)
	}
}
