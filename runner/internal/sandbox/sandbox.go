// Package sandbox 为每局对局创建全新隔离环境。
// 红线：只用平台/任务下发的内容，永不读写用户真实项目。
package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Create 新建临时目录并初始化为 git 仓库：
// 1. 新建临时目录；
// 2. git init；
// 3. 拷贝 seedDir 内容（跳过 .git、跳过符号链接等非常规文件）；
// 4. git add -A + git commit 作为判分 diff 基线。
// 任何一步失败都会清理已创建的目录并返回错误。
// 返回的 cleanup 幂等，可安全多次调用。
func Create(seedDir string) (string, func(), error) {
	sb, err := os.MkdirTemp("", "agentbattle-sandbox-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("sandbox: mktemp: %w", err)
	}
	cleanup := syncOnceRemove(sb)

	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", cleanup, err
	}

	if err := git(sb, "init"); err != nil {
		return fail(fmt.Errorf("sandbox: git init: %w", err))
	}
	if err := copyTree(seedDir, sb); err != nil {
		return fail(fmt.Errorf("sandbox: copy seed: %w", err))
	}
	if err := git(sb, "add", "-A"); err != nil {
		return fail(fmt.Errorf("sandbox: git add: %w", err))
	}
	if err := git(sb, "-c", "user.email=runner@agentbattle", "-c", "user.name=runner",
		"commit", "--allow-empty", "-m", "baseline"); err != nil {
		return fail(fmt.Errorf("sandbox: git baseline commit: %w", err))
	}
	return sb, cleanup, nil
}

// copyTree 递归拷贝 src 到 dst，跳过任意层级的 .git 与非常规文件（符号链接等）。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// seed 自带的 .git（任意层级，目录或普通文件形态）一律不拷贝，
		// 沙箱内使用全新仓库。
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Name() == ".git" {
			return nil
		}
		// 符号链接、设备等非常规条目：文件直接跳过，目录照常递归。
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// syncOnceRemove 返回幂等且并发安全的目录清理函数。
func syncOnceRemove(dir string) func() {
	var once sync.Once
	return func() {
		once.Do(func() { _ = os.RemoveAll(dir) })
	}
}

// gitDefaultTimeout 是包内所有 git 调用的默认超时，防止 git 挂起卡死 Create。
const gitDefaultTimeout = 60 * time.Second

// git 在 dir 中执行 git 子命令：
//   - 统一注入 -c core.autocrlf=false -c commit.gpgsign=false，保证 diff 字节级确定
//     且不继承宿主全局配置；
//   - 过滤 GIT_DIR/GIT_WORK_TREE 环境变量，避免误指到宿主仓库；
//   - 带 60s 超时。
func git(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitDefaultTimeout)
	defer cancel()
	full := append([]string{"-c", "core.autocrlf=false", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && (strings.EqualFold(k, "GIT_DIR") || strings.EqualFold(k, "GIT_WORK_TREE")) {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}
