// profile.go 实现 agentbattle profile 子命令：查询并打印六维能力画像。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"agentbattle/runner/internal/client"
)

// profileDim/profileDoc 是 CLI 侧的画像 JSON 视图（runner 不能导入
// platform/internal/profile——internal 边界，故本地定义最小解析形态）。
type profileDim struct {
	Score  float64 `json:"score"`
	Raw    float64 `json:"raw"`
	Sample int     `json:"sample"`
}

type profileDoc struct {
	SampleSize int                   `json:"sample_size"`
	LowSample  bool                  `json:"low_sample"`
	Dims       map[string]profileDim `json:"dims"`
}

// dimOrder 六维固定展示顺序。
var dimOrder = []string{"correctness", "debugging", "tool_efficiency", "cost", "planning", "stability"}

// cmdProfile 解析参数并打印画像。
func cmdProfile(args []string) error {
	fs := newFlagSet("profile")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	name := fs.String("name", "", "agent 名（必填）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" {
		return fmt.Errorf("--server 必填")
	}
	if *name == "" {
		return fmt.Errorf("--name 必填")
	}
	profs, err := client.New(*server).Profile(*name)
	if err != nil {
		return err
	}
	for i, p := range profs {
		var doc profileDoc
		if err := json.Unmarshal([]byte(p.ProfileJSON), &doc); err != nil {
			return fmt.Errorf("画像 %d JSON 解析失败: %w", i, err)
		}
		if i > 0 {
			fmt.Fprintln(os.Stdout)
		}
		printProfile(os.Stdout, *name, p.TaskType, doc)
	}
	if len(profs) == 0 {
		fmt.Fprintf(os.Stdout, "%s 暂无画像：完成对局后自动生成\n", *name)
	}
	return nil
}

// printProfile 以 tabwriter 输出单个 task_type 的六维表。
// dims 为空（零样本）时打印引导文案，不输出空表。
func printProfile(w io.Writer, agent, taskType string, doc profileDoc) {
	if len(doc.Dims) == 0 {
		fmt.Fprintf(w, "agent %s · 任务类型 %s 暂无画像：完成对局后自动生成\n", agent, taskType)
		return
	}
	fmt.Fprintf(w, "agent %s · 任务类型 %s · 样本 %d 局", agent, taskType, doc.SampleSize)
	if doc.LowSample {
		fmt.Fprint(w, "（样本不足，分数仅供参考）")
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "维度\t得分\t原始值\t样本")
	for _, dim := range dimOrder {
		d, ok := doc.Dims[dim]
		if !ok {
			fmt.Fprintf(tw, "%s\t—\t—\t0\n", dim)
			continue
		}
		fmt.Fprintf(tw, "%s\t%.1f\t%.2f\t%d\n", dim, d.Score, d.Raw, d.Sample)
	}
	tw.Flush()
}
