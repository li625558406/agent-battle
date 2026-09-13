// run.go 实现 agentbattle run 子命令：单局对跑。
package main

import (
	"context"
	"fmt"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/session"
)

// cmdRun 执行一局对局并打印判分摘要。
// 不传 Timeout（保持 0），由 session.Run 回退读取 task.json 的 timeout_sec。
func cmdRun(args []string) error {
	fs := newFlagSet("run")
	task := fs.String("task", "", "任务目录（必填）")
	agent := fs.String("agent", "claude-code", "agent 类型: claude-code|echo")
	label := fs.String("label", "A", "本局标签")
	out := fs.String("out", "", "报告输出目录（默认临时目录下 agentbattle-reports）")
	yolo := fs.Bool("yolo", false, "claude-code 追加 --dangerously-skip-permissions")
	var envs envFlag
	fs.Var(&envs, "env", "追加给 agent 的环境变量 K=V，可多次")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *task == "" {
		return fmt.Errorf("--task 必填")
	}
	adapt, err := pickAdapter(*agent, *yolo)
	if err != nil {
		return err
	}
	outDir, err := mustOutDir(*out)
	if err != nil {
		return err
	}
	res, err := session.Run(context.Background(), session.Config{
		Adapter:  adapt,
		TaskDir:  *task,
		Label:    *label,
		Env:      []string(envs),
		JudgeKey: sessionDevKey(),
		OutDir:   outDir,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[%s] 判分 %d/%d  耗时 %dms  报告: %s\n",
		res.Label, res.Report.Passed, res.Report.Total, res.WallMS, res.Dir)
	return nil
}

// pickAdapter 按名称构造 adapter；claude-code 立即 Detect 预检可用性。
func pickAdapter(name string, yolo bool) (adapter.Adapter, error) {
	switch name {
	case "echo":
		return adapter.Echo{}, nil
	case "claude-code":
		a := adapter.ClaudeCode{YOLO: yolo}
		if err := a.Detect(); err != nil {
			return nil, fmt.Errorf("claude-code 不可用: %w", err)
		}
		return a, nil
	default:
		return nil, fmt.Errorf("未知 agent: %q（可选 claude-code|echo）", name)
	}
}
