// Command agentbattle 是本地 agent 对战 runner 的 CLI 壳。
//
// 子命令：run（单局对跑）、mirror（A/B 镜像对战）、sign（判分包签名）、
// register（平台注册）、fetch（拉取任务包）、ladder（查看天梯）、
// profile（查看能力画像）。
package main

import (
	"fmt"
	"os"
)

const usage = `agentbattle — 本地 agent 对战 runner

用法:
  agentbattle run      --task <任务目录> [--agent claude-code|echo] [--label L]
                       [--env K=V]... [--out DIR] [--yolo]
  agentbattle mirror   --task <任务目录> [--agent claude-code|echo]
                       --env-a K=V --env-b K=V [--rounds 20] [--out DIR] [--yolo]
                       [--server URL --task-id ID --name-a N --token-a T
                        --name-b N --token-b T]
  agentbattle sign     --task <任务目录> [--key SECRET]
  agentbattle register --server URL --name X
  agentbattle fetch    --server URL --task <任务ID> --out DIR
  agentbattle ladder   --server URL
  agentbattle profile  --server URL --name X
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
	case "mirror":
		err = cmdMirror(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "register":
		err = cmdRegister(os.Args[2:])
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "ladder":
		err = cmdLadder(os.Args[2:])
	case "profile":
		err = cmdProfile(os.Args[2:])
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
