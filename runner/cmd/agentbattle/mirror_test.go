// mirror_test.go —— mirror 子命令 --dry-run 的负路径测试。
package main

import (
	"strings"
	"testing"
)

// TestMirrorDryRunNegative 互斥校验与必填校验；离线流程可达（任务预检拦截）。
func TestMirrorDryRunNegative(t *testing.T) {
	// --dry-run 与 --server 互斥
	err := cmdMirror([]string{"--task", "x", "--dry-run", "--server", "http://y"})
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("--dry-run 与 --server 应互斥: %v", err)
	}
	// 缺 --name-a/--name-b
	err = cmdMirror([]string{"--task", "x", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "--name-a") {
		t.Fatalf("--dry-run 缺名应报错: %v", err)
	}
	// 校验通过后进入既有流程（任务目录不存在 → 任务预检失败），
	// 证明 dry-run 不需要 --server 即可启动离线对战。
	// 用 --agent echo 绕开 claude-code 的本机 Detect 预检，保证断言确定性。
	err = cmdMirror([]string{"--task", "does-not-exist", "--dry-run", "--agent", "echo",
		"--name-a", "A", "--name-b", "B"})
	if err == nil || !strings.Contains(err.Error(), "任务预检失败") {
		t.Fatalf("离线流程应可达并被任务预检拦截: %v", err)
	}
}
