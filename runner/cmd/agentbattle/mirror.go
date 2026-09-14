// mirror.go 实现 agentbattle mirror 子命令：A/B 双配置镜像对战 N 局。
// 纯本地模式只出汇总报告；--server 模式每轮结束向平台创建对局并
// 上报双侧结果，平台在双侧到齐后自动完成 Elo 结算。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/client"
	"agentbattle/runner/internal/dryrun"
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
	server := fs.String("server", "", "平台 API 根地址（非空时启用联网上报）")
	taskID := fs.String("task-id", "", "平台侧任务 ID（缺省取 --task 目录名）")
	nameA := fs.String("name-a", "", "A 侧平台注册名（--server 模式必填）")
	tokA := fs.String("token-a", "", "A 侧 token（--server 模式必填）")
	nameB := fs.String("name-b", "", "B 侧平台注册名（--server 模式必填）")
	tokB := fs.String("token-b", "", "B 侧 token（--server 模式必填）")
	fixA := fs.String("fix-a", "", "A 侧 echo 解法内容（--agent echo 专用，制造确定性判分差）")
	fixB := fs.String("fix-b", "", "B 侧 echo 解法内容（--agent echo 专用）")
	dryRun := fs.Bool("dry-run", false, "影子赛：流程照常但请求发往本地假服务端并导出全部 payload（与 --server 互斥）")
	var envA, envB envFlag
	fs.Var(&envA, "env-a", "A 侧环境变量 K=V，可多次")
	fs.Var(&envB, "env-b", "B 侧环境变量 K=V，可多次")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *task == "" {
		return fmt.Errorf("--task 必填")
	}
	if *rounds <= 0 {
		return fmt.Errorf("--rounds 必须 > 0: %d", *rounds)
	}
	// 分侧解法注入仅对 echo 有意义（claude-code 的配置分化走 --env-a/b）
	if (*fixA != "" || *fixB != "") && *agent != "echo" {
		return fmt.Errorf("--fix-a/--fix-b 仅支持 --agent echo")
	}
	// 影子赛与联网互斥：dry-run 的承诺就是零出网，给了 --server 只会让
	// 用户误以为数据发去了平台
	if *dryRun && *server != "" {
		return fmt.Errorf("--dry-run 与 --server 互斥（影子赛零出网）")
	}
	// 联网上报模式参数校验：缺一即拦在开赛前，避免跑到一半才发现没法上报
	var cl *client.Client
	var ds *dryrun.Server
	tid := *taskID
	if *server != "" {
		if *nameA == "" || *tokA == "" || *nameB == "" || *tokB == "" {
			return fmt.Errorf("--server 模式需要 --name-a/--token-a/--name-b/--token-b")
		}
		if tid == "" {
			tid = filepath.Base(*task)
		}
		cl = client.New(*server)
	}
	// 影子赛：本地起假服务端，代发两次注册取占位 token，client 全部
	// 流量指向 127.0.0.1——mirror 主流程零改动
	if *dryRun {
		if *nameA == "" || *nameB == "" {
			return fmt.Errorf("--dry-run 模式需要 --name-a/--name-b（将作为注册 payload 的真实字段）")
		}
		if *nameA == *nameB {
			return fmt.Errorf("--name-a 与 --name-b 不能相同（同名注册无法分侧归因，导出文件将缺失一侧）")
		}
		if tid == "" {
			tid = filepath.Base(*task)
		}
	}
	// 预检 agent 可用性，避免开赛后才发现本机不可用
	if _, err := pickAdapter(*agent, *yolo); err != nil {
		return err
	}
	outDir, err := mustOutDir(*out)
	if err != nil {
		return err
	}
	tokA2, tokB2 := *tokA, *tokB
	if *dryRun {
		// 建服前先做任务预检：影子赛的承诺是"预检失败不产出半套 dump"，
		// 若放到 session.Mirror 内部才拦截，此处已代发注册并落了 2 个 register 文件
		if err := session.PrecheckTask(*task, sessionDevKey()); err != nil {
			return err
		}
		d, err := dryrun.NewServer(dryrun.Options{OutDir: outDir, NameA: *nameA, NameB: *nameB})
		if err != nil {
			return err
		}
		defer d.Close()
		ds = d
		dcl := client.New(d.Base())
		if _, tok, err := dcl.Register(*nameA); err != nil {
			return fmt.Errorf("dry-run 注册 %s 失败: %w", *nameA, err)
		} else {
			tokA2 = tok
		}
		if _, tok, err := dcl.Register(*nameB); err != nil {
			return fmt.Errorf("dry-run 注册 %s 失败: %w", *nameB, err)
		} else {
			tokB2 = tok
		}
		cl = dcl
	}
	mk := func() adapter.Adapter {
		a, err := pickAdapter(*agent, *yolo)
		if err != nil {
			return nil // 已预检通过，理论上不可达；nil 会在局内以错误计
		}
		return a
	}
	// 分侧构造：fix 非空时该侧注入解法（确定性满分），否则维持统一 mk
	side := func(fix string) func() adapter.Adapter {
		if fix == "" {
			return mk
		}
		return func() adapter.Adapter { return adapter.Echo{FixContent: fix} }
	}
	cfg := session.MirrorConfig{
		TaskDir:  *task,
		JudgeKey: sessionDevKey(),
		Rounds:   *rounds,
		OutDir:   outDir,
		EnvA:     []string(envA),
		EnvB:     []string(envB),
		MakeA:    side(*fixA),
		MakeB:    side(*fixB),
	}
	if cl != nil {
		cfg.Report = func(_ context.Context, r int, resA, resB session.Result, errA, errB error) error {
			return reportRound(os.Stdout, cl, r, tokA2, tokB2, tid, *nameA, *nameB, resA, resB, errA, errB)
		}
	}
	sum, err := session.Mirror(context.Background(), cfg)
	if err != nil {
		return err
	}
	fmt.Printf("镜像对战完成: A 胜 %d | B 胜 %d | 平 %d | A崩 %d | B崩 %d\n",
		sum.WinsA, sum.WinsB, sum.Ties, sum.AErrors, sum.BErrors)
	fmt.Printf("明细: %s\n", outDir)
	// 影子赛：逐条打印拦截摘要 + 隐私对照声明；不拉天梯（平台→runner
	// 方向不属于"平台会收到的数据"）
	if *dryRun {
		for _, rec := range ds.Records() {
			fmt.Printf("  [%02d] %s %s → %s  %s\n", rec.Seq, rec.Method, rec.Path, rec.File, rec.Note)
		}
		if err := ds.DumpError(); err != nil {
			return fmt.Errorf("dry-run 导出失败: %w", err)
		}
		fmt.Printf("payload 已导出: %s\n", ds.Dir())
		fmt.Println("影子赛完成：以上即平台将收到的全部数据。全程未出网；事件流仅含路径与操作类型，不含文件内容与用户配置（CLAUDE.md/skills/记忆永不上传）。")
		return nil
	}
	// 联网模式：全部轮次结束后拉取天梯前 5 名展示结算成果。
	// 拉取失败只警告不改退出码——此时全部轮次上报已成功、Elo 已入账。
	if cl != nil {
		fmt.Println("天梯前 5:")
		if err := runLadder(os.Stdout, *server, 5); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 拉取天梯失败（结算不受影响）: %v\n", err)
		}
	}
	return nil
}

// reportRound 向平台上报一轮对战：以 A 侧 token 创建对局（mirror 每轮 =
// 平台一场 match），随后依次上传 A、B 两侧结果；双侧到齐时平台返回结算
//（Elo 已更新），否则返回 waiting（等待对方上报）。
// errA/errB 非 nil 的侧按 0/0、空 diff_hash 上报（技术性失败按零通过，
// 双零平局规则兜底）；任何上报失败返回 error（Report 钩子语义：中止整场）。
func reportRound(w io.Writer, cl *client.Client, round int,
	tokA, tokB, taskID, nameA, nameB string,
	resA, resB session.Result, errA, errB error) error {
	matchID, _, err := cl.CreateMatch(tokA, taskID, nameA, nameB)
	if err != nil {
		return fmt.Errorf("第 %d 轮创建对局失败: %w", round, err)
	}
	for _, s := range []struct {
		side, tok, name string
		res             session.Result
		runErr          error
	}{
		{"a", tokA, nameA, resA, errA},
		{"b", tokB, nameB, resB, errB},
	} {
		in := client.ResultIn{Side: s.side}
		if s.runErr == nil {
			in.Passed, in.Total = s.res.Report.Passed, s.res.Report.Total
			in.WallMS = s.res.WallMS
			in.DiffHash = s.res.Report.DiffHash
			gz, gerr := client.GzipEvents(s.res.Events)
			if gerr != nil {
				return fmt.Errorf("第 %d 轮 %s 侧事件流压缩失败: %w", round, s.side, gerr)
			}
			in.EventsGZ = gz
		}
		settle, err := cl.UploadResult(s.tok, matchID, in)
		if err != nil {
			return fmt.Errorf("第 %d 轮 %s 侧(%s)上报失败: %w", round, s.side, s.name, err)
		}
		if settle.Status == "waiting" {
			fmt.Fprintf(w, "第 %d 轮 %s 侧已上报，等待对方上报\n", round, s.side)
			continue
		}
		fmt.Fprintf(w, "第 %d 轮 结算 winner=%s A分=%.0f B分=%.0f\n",
			round, settle.Winner, settle.RatingA, settle.RatingB)
	}
	return nil
}
