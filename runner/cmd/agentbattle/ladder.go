// ladder.go 实现 agentbattle ladder 子命令：打印平台天梯表格。
package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"agentbattle/runner/internal/client"
)

// cmdLadder 解析参数并打印完整天梯。
func cmdLadder(args []string) error {
	fs := newFlagSet("ladder")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" {
		return fmt.Errorf("--server 必填")
	}
	return runLadder(os.Stdout, *server, 0)
}

// runLadder 拉取天梯并打印；limit > 0 时只打印前 limit 行。
// 空榜打印提示（非错误）。每行格式：名称 评分 局数 胜 负 平。
func runLadder(w io.Writer, server string, limit int) error {
	rows, err := client.New(server).Ladder()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "天梯暂无数据：先注册 agent 并完成对局")
		return nil
	}
	printLadder(w, rows, limit)
	return nil
}

// printLadder 以 tabwriter 对齐输出天梯表格。
func printLadder(w io.Writer, rows []client.LadderRow, limit int) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "名称\t评分\t局数\t胜\t负\t平")
	for i, r := range rows {
		if limit > 0 && i >= limit {
			break
		}
		fmt.Fprintf(tw, "%s\t%.0f\t%d\t%d\t%d\t%d\n",
			r.Name, r.Rating, r.Games, r.Wins, r.Losses, r.Ties)
	}
	tw.Flush()
}
