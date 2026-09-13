// register.go 实现 agentbattle register 子命令：向平台注册 agent 并打印 token。
// M1 不落盘 token（敏感信息），由用户自行保存。
package main

import (
	"fmt"
	"io"
	"os"

	"agentbattle/runner/internal/client"
)

// cmdRegister 解析参数并执行注册。
func cmdRegister(args []string) error {
	fs := newFlagSet("register")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	name := fs.String("name", "", "agent 名称（必填）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	return runRegister(os.Stdout, *server, *name)
}

// runRegister 调用平台注册接口，打印注册成功信息（含名称与 id）和
// "token: <tok>" 行（E2E 以此行解析 token）。
func runRegister(w io.Writer, server, name string) error {
	if server == "" {
		return fmt.Errorf("--server 必填")
	}
	if name == "" {
		return fmt.Errorf("--name 必填")
	}
	id, token, err := client.New(server).Register(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "注册成功: %s (id=%d)\n", name, id)
	fmt.Fprintf(w, "token: %s\n", token)
	return nil
}
