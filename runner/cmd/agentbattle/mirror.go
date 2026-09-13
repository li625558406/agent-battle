// mirror.go 实现 agentbattle mirror 子命令：A/B 双配置镜像对战 N 局。
package main

import (
	"context"
	"fmt"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/session"
)

// cmdMirror 以同一 agent 类型的两个环境配置（--env-a / --env-b）对战 N 局。
func cmdMirror(args []string) error {
	fs := newFlagSet("mirror")
	task := fs.String("task", "", "任务目录（必填）")
	agent := fs.String("agent", "claude-code", "agent 类型: claude-code|echo")
	rounds := fs.Int("rounds", 20, "对战场次")
	out := fs.String("out", "", "报告输出目录（默认临时目录下 agentbattle-reports）")
	yolo := fs.Bool("yolo", false, "claude-code 追加 --dangerously-skip-permissions")
	var envA, envB envFlag
	fs.Var(&envA, "env-a", "A 侧环境变量 K=V，可多次")
	fs.Var(&envB, "env-b", "B 侧环境变量 K=V，可多次")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *task == "" {
		return fmt.Errorf("--task 必填")
	}
	if *rounds <= 0 {
		return fmt.Errorf("--rounds 必须 > 0: %d", *rounds)
	}
	// 预检 agent 可用性，避免开赛后才发现本机不可用
	if _, err := pickAdapter(*agent, *yolo); err != nil {
		return err
	}
	outDir, err := mustOutDir(*out)
	if err != nil {
		return err
	}
	mk := func() adapter.Adapter {
		a, err := pickAdapter(*agent, *yolo)
		if err != nil {
			return nil // 已预检通过，理论上不可达；nil 会在局内以错误计
		}
		return a
	}
	sum, err := session.Mirror(context.Background(), session.MirrorConfig{
		TaskDir:  *task,
		JudgeKey: sessionDevKey(),
		Rounds:   *rounds,
		OutDir:   outDir,
		EnvA:     []string(envA),
		EnvB:     []string(envB),
		MakeA:    mk,
		MakeB:    mk,
	})
	if err != nil {
		return err
	}
	fmt.Printf("镜像对战完成: A 胜 %d | B 胜 %d | 平 %d | A崩 %d | B崩 %d\n",
		sum.WinsA, sum.WinsB, sum.Ties, sum.AErrors, sum.BErrors)
	fmt.Printf("明细: %s\n", outDir)
	return nil
}
