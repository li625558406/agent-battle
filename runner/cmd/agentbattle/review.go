// review.go 实现 agentbattle review 子命令：按 matchID 查询并打印对局复盘。
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"agentbattle/runner/internal/client"
)

// cmdReview 解析参数并调 runReview 打印复盘。
func cmdReview(args []string) error {
	fs := newFlagSet("review")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	match := fs.Int64("match", 0, "对局 ID（必填，正整数）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" {
		return fmt.Errorf("--server 必填")
	}
	if *match <= 0 {
		return fmt.Errorf("--match 必填（正整数）")
	}
	return runReview(os.Stdout, *server, *match)
}

// runReview 拉取复盘并打印。
func runReview(w io.Writer, server string, matchID int64) error {
	rep, err := client.New(server).Review(matchID)
	if err != nil {
		return err
	}
	printReview(w, rep)
	return nil
}

// printReview 渲染复盘：结论行 + 双侧概要 + 对比表 + 双侧时间线。
// 无事件侧的 event 类对比指标渲染 "—"（与 profile 的缺维占位一致）。
func printReview(w io.Writer, rep client.ReviewReport) {
	winner := "平局"
	if rep.Winner == "a" || rep.Winner == "b" {
		if s, ok := rep.Sides[rep.Winner]; ok {
			winner = s.Agent
		}
	}
	fmt.Fprintf(w, "对局 %d · %s（%s）· 胜者 %s\n",
		rep.MatchID, rep.TaskID, rep.TaskType, winner)
	for _, side := range []string{"a", "b"} {
		s, ok := rep.Sides[side]
		if !ok {
			continue
		}
		ev := "无"
		if s.HasEvents {
			ev = "有"
		}
		fmt.Fprintf(w, "  %s %s: %d/%d wall %dms 事件流 %s\n",
			strings.ToUpper(side), s.Agent, s.Passed, s.Total, s.WallMS, ev)
	}

	hasEvents := func(side string) bool {
		s, ok := rep.Sides[side]
		return ok && s.HasEvents
	}
	cell := func(v float64, has bool) string {
		if !has {
			return "—"
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	evA, evB := hasEvents("a"), hasEvents("b")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "指标\tA\tB")
	fmt.Fprintf(tw, "通过率\t%.2f\t%.2f\n", rep.Compare.PassRatio.A, rep.Compare.PassRatio.B)
	fmt.Fprintf(tw, "tool_call\t%s\t%s\n", cell(rep.Compare.ToolCalls.A, evA), cell(rep.Compare.ToolCalls.B, evB))
	fmt.Fprintf(tw, "error\t%s\t%s\n", cell(rep.Compare.Errors.A, evA), cell(rep.Compare.Errors.B, evB))
	fmt.Fprintf(tw, "file_edit\t%s\t%s\n", cell(rep.Compare.Edits.A, evA), cell(rep.Compare.Edits.B, evB))
	fmt.Fprintf(tw, "tokens\t%s\t%s\n", cell(rep.Compare.Tokens.A, evA), cell(rep.Compare.Tokens.B, evB))
	fmt.Fprintf(tw, "wall_ms\t%.0f\t%.0f\n", rep.Compare.WallMS.A, rep.Compare.WallMS.B)
	tw.Flush()

	for _, side := range []string{"a", "b"} {
		evs := rep.Timeline[side]
		star := map[int]bool{}
		for _, mk := range rep.Marks[side] {
			if mk.Kind == "first_error" {
				star[mk.Seq] = true
			}
		}
		agent := side
		if s, ok := rep.Sides[side]; ok {
			agent = s.Agent
		}
		fmt.Fprintf(w, "时间线 %s（★=首次报错）:\n", agent)
		if len(evs) == 0 {
			fmt.Fprintln(w, "  （无事件流）")
			continue
		}
		for _, e := range evs {
			line := fmt.Sprintf("  #%d %s", e.Seq, e.Type)
			if e.Tool != "" {
				line += " " + e.Tool
			}
			if e.Path != "" {
				line += " " + e.Path
			}
			if e.DurationMS > 0 {
				line += fmt.Sprintf(" %dms", e.DurationMS)
			}
			if e.Tokens > 0 {
				line += fmt.Sprintf(" %dtok", e.Tokens)
			}
			if star[e.Seq] {
				line += " ★"
			}
			fmt.Fprintln(w, line)
		}
	}
}
