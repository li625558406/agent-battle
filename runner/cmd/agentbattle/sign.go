// sign.go 实现 agentbattle sign 子命令：为任务判分包生成 HMAC 签名。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"agentbattle/runner/internal/judge"
)

// cmdSign 对 taskDir/tests/manifest.json 签名并写入 taskDir/tests/sig。
func cmdSign(args []string) error {
	fs := newFlagSet("sign")
	task := fs.String("task", "", "任务目录（必填）")
	key := fs.String("key", string(sessionDevKey()), "HMAC key（默认本地开发 key）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *task == "" {
		return fmt.Errorf("--task 必填")
	}
	if err := judge.SignDir(*task, []byte(*key)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "已签名: %s\n", filepath.Join(*task, "tests", "sig"))
	return nil
}
