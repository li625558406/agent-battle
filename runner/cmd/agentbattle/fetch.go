// fetch.go 实现 agentbattle fetch 子命令：从平台拉取任务包并安全解压。
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentbattle/runner/internal/client"
)

// cmdFetch 解析参数并执行任务包拉取与解压。
func cmdFetch(args []string) error {
	fs := newFlagSet("fetch")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	task := fs.String("task", "", "任务 ID（必填）")
	out := fs.String("out", "", "解压目标目录（必填）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	return runFetch(os.Stdout, *server, *task, *out)
}

// runFetch 拉取任务包 zip，解压到 out 的绝对路径（ExtractBundle 内部
// 做 zip slip 防护），成功后打印任务 ID 与目标目录。
func runFetch(w io.Writer, server, task, out string) error {
	if server == "" {
		return fmt.Errorf("--server 必填")
	}
	if task == "" {
		return fmt.Errorf("--task 必填")
	}
	if out == "" {
		return fmt.Errorf("--out 必填")
	}
	zipBytes, err := client.New(server).FetchBundle(task)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("解析输出目录失败: %w", err)
	}
	if err := client.ExtractBundle(zipBytes, abs); err != nil {
		return err
	}
	fmt.Fprintf(w, "任务包 %s 已解压到 %s\n", task, abs)
	return nil
}
