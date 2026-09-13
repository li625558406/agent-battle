// mirror.go 实现 A/B 镜像对战：同一任务下两套配置各跑 N 局，按
// 通过数 → 耗时 → 平局 决出每局胜负，产出汇总与逐局明细。
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/judge"
)

// MirrorConfig 描述一场镜像对战。
// MakeA/MakeB 每局各调用一次，产出该局的 agent 实例（配置分化的载体）。
type MirrorConfig struct {
	TaskDir  string
	JudgeKey []byte
	Rounds   int
	OutDir   string
	EnvA     []string
	EnvB     []string
	MakeA    func() adapter.Adapter
	MakeB    func() adapter.Adapter
}

// RoundDetail 单局明细。
type RoundDetail struct {
	Round  int    `json:"round"`
	Winner string `json:"winner"` // "a" | "b" | "tie" | "error"
	PassA  int    `json:"pass_a"`
	TotalA int    `json:"total_a"`
	PassB  int    `json:"pass_b"`
	TotalB int    `json:"total_b"`
	WallA  int64  `json:"wall_a"`
	WallB  int64  `json:"wall_b"`
	DirA   string `json:"dir_a"`
	DirB   string `json:"dir_b"`
}

// Summary 镜像对战汇总与明细。
type Summary struct {
	TaskID  string        `json:"task_id"`
	Rounds  int           `json:"rounds"`
	WinsA   int           `json:"wins_a"`
	WinsB   int           `json:"wins_b"`
	Ties    int           `json:"ties"`
	AErrors int           `json:"a_errors"`
	BErrors int           `json:"b_errors"`
	Details []RoundDetail `json:"details"`
}

// RoundsPlayed 返回已完成的场次数（分出胜负 + 平局，不含取消中断）。
func (s Summary) RoundsPlayed() int { return s.WinsA + s.WinsB + s.Ties }

// Mirror 顺序跑 N 局，每局 A、B 各一次 session.Run，返回汇总。
// 单局单侧失败计入该侧错误并按规则判给对方；汇总写入 OutDir 后返回。
func Mirror(ctx context.Context, cfg MirrorConfig) (Summary, error) {
	if cfg.Rounds <= 0 {
		return Summary{}, fmt.Errorf("Rounds 必须 > 0: %d", cfg.Rounds)
	}
	if cfg.MakeA == nil || cfg.MakeB == nil {
		return Summary{}, fmt.Errorf("MakeA/MakeB 未设置")
	}
	// 任务级预检，fail-fast：task.json 缺失/损坏、判分包缺失或验签不通过
	// 属于任务配置错误，与 agent 无关。若不拦截，每局会在沙箱创建前失败并被
	// 一律按 "该侧 agent 崩溃" 计入统计，最终静默产出垃圾汇总且 exit 0。
	task, err := loadTask(cfg.TaskDir)
	if err != nil {
		return Summary{}, fmt.Errorf("任务预检失败: %w", err)
	}
	if err := judge.VerifyDir(cfg.TaskDir, cfg.JudgeKey); err != nil {
		return Summary{}, fmt.Errorf("判分包预检失败: %w", err)
	}
	taskID := task.TaskID
	sum := Summary{TaskID: taskID, Rounds: cfg.Rounds,
		Details: make([]RoundDetail, 0, cfg.Rounds)}

	for r := 1; r <= cfg.Rounds; r++ {
		if err := ctx.Err(); err != nil {
			return abort(sum, cfg.OutDir,
				fmt.Errorf("镜像对战被取消(已完成 %d 局): %w", sum.RoundsPlayed(), err))
		}
		label := fmt.Sprintf("A-r%d", r)
		resA, errA := Run(ctx, Config{Adapter: cfg.MakeA(), TaskDir: cfg.TaskDir,
			Label: label, Env: cfg.EnvA, JudgeKey: cfg.JudgeKey, OutDir: cfg.OutDir})
		label = fmt.Sprintf("B-r%d", r)
		resB, errB := Run(ctx, Config{Adapter: cfg.MakeB(), TaskDir: cfg.TaskDir,
			Label: label, Env: cfg.EnvB, JudgeKey: cfg.JudgeKey, OutDir: cfg.OutDir})

		// ctx 取消/超时导致的错误是"对局被中止"，不是 agent 技术性失败：
		// 不计入崩溃统计；当局未完整判分，不产生明细；已完成的局数据
		// 以部分汇总形式落盘后原样返回 ctx 错误。
		// 注意区分：session.Run 的单局自身超时返回的 "agent 执行超时" 不包装
		// context.DeadlineExceeded，仍走下方 switch 计为对应侧错误。
		if ctx.Err() != nil || errors.Is(errA, context.Canceled) || errors.Is(errA, context.DeadlineExceeded) ||
			errors.Is(errB, context.Canceled) || errors.Is(errB, context.DeadlineExceeded) {
			cause := ctx.Err()
			if cause == nil {
				// 理论不可达（Run 仅在 ctx.Err() 非 nil 时包装 ctx 错误），
				// 兜底取 Run 的错误，避免静默返回 nil
				cause = errA
				if cause == nil {
					cause = errB
				}
			}
			return abort(sum, cfg.OutDir, cause)
		}

		d := RoundDetail{Round: r, DirA: resA.Dir, DirB: resB.Dir}
		switch {
		case errA != nil && errB != nil:
			d.Winner = "error"
			sum.AErrors++
			sum.BErrors++
		case errA != nil:
			d.Winner = "b"
			sum.AErrors++
			sum.WinsB++
		case errB != nil:
			d.Winner = "a"
			sum.BErrors++
			sum.WinsA++
		default:
			d.Winner = winner(resA, resB)
			switch d.Winner {
			case "a":
				sum.WinsA++
			case "b":
				sum.WinsB++
			default:
				sum.Ties++
			}
		}
		// 错误路径下对应侧的 Report 为零值；若失败发生在沙箱创建前（如
		// adapter Detect 失败），Dir 还是空串（无取证目录），如实记录即可
		d.PassA, d.TotalA = resA.Report.Passed, resA.Report.Total
		d.PassB, d.TotalB = resB.Report.Passed, resB.Report.Total
		d.WallA, d.WallB = resA.WallMS, resB.WallMS
		sum.Details = append(sum.Details, d)
	}

	if err := persistSummary(sum, cfg.OutDir); err != nil {
		return sum, fmt.Errorf("写入汇总失败: %w", err)
	}
	return sum, nil
}

// abort 对局被中止时的收尾：落盘含已完成局的部分汇总后返回携带原因的错误；
// 落盘失败不掩盖中止原因，用 errors.Join 一并上报。
func abort(sum Summary, outDir string, cause error) (Summary, error) {
	if perr := persistSummary(sum, outDir); perr != nil {
		return sum, errors.Join(cause, perr)
	}
	return sum, cause
}

// winner 决出单局胜者：
// 双方均 0 通过 → 平局（失败的耗时没有竞速意义）；
// 通过比例高者胜 → 同分比 WallMS 短者胜 → 再同分平局。
// 注意：本规则在平台侧 platform/internal/store（winnerOf）各有一份
//（跨 internal 边界不可导入），规则变更必须双侧同步。
func winner(a, b Result) string {
	if a.Report.Passed == 0 && b.Report.Passed == 0 {
		return "tie"
	}
	sa, sb := score(a), score(b)
	if sa > sb {
		return "a"
	}
	if sa < sb {
		return "b"
	}
	switch {
	case a.WallMS < b.WallMS:
		return "a"
	case b.WallMS < a.WallMS:
		return "b"
	}
	return "tie"
}

// score 计算通过比例，Total 为 0（无测试）时记 0。
func score(r Result) float64 {
	if r.Report.Total == 0 {
		return 0
	}
	return float64(r.Report.Passed) / float64(r.Report.Total)
}

// persistSummary 将汇总以 MarshalIndent 写入
// outDir/mirror-<taskid>-<unixnano>.json；outDir 为空则落临时目录。
func persistSummary(s Summary, outDir string) error {
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "agentbattle-reports")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(outDir, fmt.Sprintf("mirror-%s-%d.json", s.TaskID, time.Now().UnixNano()))
	return os.WriteFile(p, b, 0o644)
}
