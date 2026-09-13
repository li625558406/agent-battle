// env.go 收敛 CLI 各子命令共用的 flag 构造与环境变量参数解析。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentbattle/runner/internal/session"
)

// newFlagSet 创建统一输出到 stderr、出错即返回的 FlagSet。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// sessionDevKey 返回本地开发默认判分 key（与 session.DevJudgeKey 一致）。
func sessionDevKey() []byte { return []byte(session.DevJudgeKey) }

// filepathJoin 规范化拼接路径（供 CLI 层组合用户输入的目录）。
func filepathJoin(elem ...string) string {
	return filepath.Join(elem...)
}

// envFlag 收集多次出现的 K=V 参数（如 --env CFG=A --env X=1）。
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }

// Set 校验必须含 "=" 且不以 "=" 开头（即 K 非空），否则拒绝。
func (e *envFlag) Set(v string) error {
	if !strings.Contains(v, "=") || strings.HasPrefix(v, "=") {
		return fmt.Errorf("--env 必须为 K=V 形式: %q", v)
	}
	*e = append(*e, v)
	return nil
}

// mustOutDir 归一化报告输出目录：为空则落到系统临时目录下的
// agentbattle-reports，并确保目录存在。
func mustOutDir(dir string) (string, error) {
	if dir == "" {
		dir = filepathJoin(os.TempDir(), "agentbattle-reports")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建输出目录 %s: %w", dir, err)
	}
	return dir, nil
}
