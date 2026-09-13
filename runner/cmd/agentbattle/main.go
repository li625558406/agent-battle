// Command agentbattle 是本地 agent 对战 runner 的 CLI 壳。
//
// 子命令：run（单局对跑）、mirror（A/B 镜像对战）、sign（判分包签名）。
package main

import (
	"fmt"
	"os"
)

const usage = `agentbattle — 本地 agent 对战 runner

用法:
  agentbattle run    --task <任务目录> [--agent claude-code|echo] [--label L]
                     [--env K=V]... [--out DIR] [--yolo]
  agentbattle mirror --task <任务目录> [--agent claude-code|echo]
                     --env-a K=V --env-b K=V [--rounds 20] [--out DIR] [--yolo]
  agentbattle sign   --task <任务目录> [--key SECRET]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentbattle %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
