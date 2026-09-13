// env.go 收敛 CLI 各子命令共用的 flag 构造与环境变量参数解析。
package main

import (
	"errors"
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

// parseFlags 解析子命令参数，返回 (是否为帮助请求, 错误)。
// 用户主动 -h/--help 时 flag 包打印 usage 并返回 flag.ErrHelp，此时
// helped=true，调用方应立即返回 nil（上层 exit 0），不得继续校验必填参数。
func parseFlags(fs *flag.FlagSet, args []string) (bool, error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	return false, nil
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
// 重复 K 覆盖旧值（保留最后出现的值），与 Go 子进程环境的去重语义
// （同名保最后）一致，且调用方传入的切片所见即所得。
func (e *envFlag) Set(v string) error {
	if !strings.Contains(v, "=") || strings.HasPrefix(v, "=") {
		return fmt.Errorf("--env 必须为 K=V 形式: %q", v)
	}
	k, _, _ := strings.Cut(v, "=")
	for i, old := range *e {
		if ok, _, _ := strings.Cut(old, "="); strings.EqualFold(ok, k) {
			(*e)[i] = v
			return nil
		}
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
