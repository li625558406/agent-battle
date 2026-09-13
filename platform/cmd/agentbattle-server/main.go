// agentbattle-server：平台 M1 最小服务端。
// 提供 agent 注册 / 对局创建 / 结果上报（Elo 结算）/ 天梯 / 任务包 zip 分发。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"agentbattle/platform/internal/api"
	"agentbattle/platform/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	tasksDir := flag.String("tasks", "./examples", "任务目录根")
	storePath := flag.String("store", "agentbattle.db", "SQLite 库文件路径")
	judgeKey := flag.String("judge-key", "dev-secret", "裁判密钥（下发给对局双方用于 judge 验签）")
	flag.Parse()

	st, err := store.Open(*storePath)
	if err != nil {
		log.Fatalf("打开存储 %s 失败: %v", *storePath, err)
	}
	defer st.Close()

	// 任务目录缺失只警告不退出：注册/天梯仍可用，仅创建对局与 bundle 不可用
	if fi, err := os.Stat(*tasksDir); err != nil || !fi.IsDir() {
		log.Printf("警告: 任务目录 %s 不存在或不是目录，创建对局/任务包接口将不可用", *tasksDir)
	}

	log.Printf("agentbattle-server 监听 %s（tasks=%s store=%s）", *addr, *tasksDir, *storePath)
	if err := http.ListenAndServe(*addr, api.New(st, *tasksDir, []byte(*judgeKey))); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
		os.Exit(1)
	}
}
