// Package session 编排一局对局：prepare(沙箱) → execute(agent) → judge → report。
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/collector"
	"agentbattle/runner/internal/judge"
	"agentbattle/runner/internal/sandbox"
)

// DevJudgeKey 与 judge 包测试共用；平台化后由平台按局下发。
const DevJudgeKey = "dev-secret"

type Config struct {
	Adapter  adapter.Adapter
	TaskDir  string
	Label    string
	Env      []string
	Timeout  time.Duration // 0 = 读 task.json 的 TimeoutSec
	JudgeKey []byte
	OutDir   string
}

type Result struct {
	Label  string
	Dir    string
	Report protocol.JudgeReport
	Events []protocol.Event
	WallMS int64
}

// Run 执行一局。沙箱在结束后删除工作副本，报告目录保留。
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Adapter == nil {
		return Result{}, fmt.Errorf("Adapter 未设置")
	}
	if cfg.Timeout < 0 {
		return Result{}, fmt.Errorf("Timeout 不能为负: %s", cfg.Timeout)
	}
	task, err := loadTask(cfg.TaskDir)
	if err != nil {
		return Result{}, err
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = time.Duration(task.TimeoutSec) * time.Second
	}
	if timeout == 0 {
		timeout = 600 * time.Second
	}
	if err := cfg.Adapter.Detect(); err != nil {
		return Result{}, fmt.Errorf("agent 不可用: %w", err)
	}

	sb, cleanupSb, err := sandbox.Create(filepath.Join(cfg.TaskDir, "seed"))
	if err != nil {
		return Result{}, err
	}
	defer cleanupSb()

	// 在 agent 启动前记录基线 HEAD SHA：这是执行环境自己记得、agent 无法
	// 伪造的值，判分命令据此校验提交历史未被改写（防 commit --amend 架空
	// 工作树 diff 校验）。失败即终止本局——没有可信基线，判分不可信。
	baselineSHA, err := sandbox.HeadRev(sb)
	if err != nil {
		return Result{}, fmt.Errorf("记录基线 HEAD 失败: %w", err)
	}

	col := collector.New()
	events := make(chan adapter.RawEvent, 256)
	execDone := make(chan error, 1)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	go func() {
		err := cfg.Adapter.Launch(runCtx, sb, task.Description, cfg.Env, events)
		// 先发 execDone 再 close(events)：保证主循环先拿到错误，
		// 再观察到渠道关闭（契约：out 由调用方 close，Launch 返回后不再有 send）。
		execDone <- err
		close(events)
	}()

	// classify 归类失败原因：runCtx 已取消/超时（含 adapter 遵守 ctx 返回
	// context.DeadlineExceeded 的情况）按超时/取消报告，否则按 agent 执行失败。
	classify := func(launchErr error) error {
		if runCtx.Err() != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("对局上下文被取消: %w", ctx.Err())
			}
			return fmt.Errorf("agent 执行超时(%s)", timeout)
		}
		return fmt.Errorf("agent 执行失败: %w", launchErr)
	}
	// fail 统一处理崩溃/超时路径：补记 EventError 并把已采集事件落盘取证。
	fail := func(execErr error) (Result, error) {
		col.Add(adapter.RawEvent{Type: protocol.EventError, Note: "agent 异常退出"})
		dir, werr := writeReport(cfg, task, protocol.JudgeReport{}, col,
			time.Since(start).Milliseconds(), execErr)
		if werr != nil {
			// 落盘失败不掩盖原始错误
			return Result{}, fmt.Errorf("%w（另：取证报告写入失败: %v）", execErr, werr)
		}
		return Result{Dir: dir}, execErr
	}

agentLoop:
	for {
		select {
		case raw, ok := <-events:
			if !ok {
				// close 在 execDone 发送之后执行，能观察到关闭说明 execDone 必有值
				if err := <-execDone; err != nil {
					return fail(classify(err))
				}
				break agentLoop
			}
			col.Add(raw)
		case err := <-execDone:
			// Launch 已返回，events 随即关闭；排干剩余缓冲事件
			for raw := range events {
				col.Add(raw)
			}
			if err != nil {
				return fail(classify(err))
			}
			break agentLoop
		case <-runCtx.Done():
			// 超时/取消：尽力排干已缓冲事件用于取证。
			// Launch 仍在运行，不能 close(events)；返回后 defer cancel()
			// 使 runCtx 取消，adapter 按契约杀死 agent 进程，Launch 返回，
			// goroutine 写入缓冲容量为 1 的 execDone 并 close(events) 后结束，不泄漏。
		drain:
			for {
				select {
				case raw, ok := <-events:
					if !ok {
						break drain
					}
					col.Add(raw)
				default:
					break drain
				}
			}
			return fail(classify(nil))
		}
	}
	wall := time.Since(start).Milliseconds()

	rep, err := judge.Run(cfg.TaskDir, sb, baselineSHA, cfg.JudgeKey)
	if err != nil {
		return Result{}, fmt.Errorf("判分失败: %w", err)
	}

	dir, err := writeReport(cfg, task, rep, col, wall, nil)
	if err != nil {
		return Result{}, err
	}
	return Result{Label: cfg.Label, Dir: dir, Report: rep, Events: col.Events(), WallMS: wall}, nil
}

func loadTask(taskDir string) (protocol.TaskManifest, error) {
	var t protocol.TaskManifest
	b, err := os.ReadFile(filepath.Join(taskDir, "task.json"))
	if err != nil {
		return t, fmt.Errorf("读 task.json: %w", err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("解析 task.json: %w", err)
	}
	return t, nil
}

// writeReport 落盘报告。execErr 非 nil 表示崩溃/超时路径：
// 未执行判分，passed/total 记零值，并在 summary 中附带错误信息供取证。
func writeReport(cfg Config, task protocol.TaskManifest, rep protocol.JudgeReport,
	col *collector.Collector, wallMS int64, execErr error) (string, error) {
	out := cfg.OutDir
	if out == "" {
		out = filepath.Join(os.TempDir(), "agentbattle-reports")
	}
	dir := filepath.Join(out, fmt.Sprintf("%s-%s-%d", task.TaskID, cfg.Label, time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	summary := map[string]interface{}{
		"task_id": task.TaskID, "label": cfg.Label, "wall_ms": wallMS,
		"passed": rep.Passed, "total": rep.Total,
	}
	if execErr != nil {
		summary["error"] = execErr.Error()
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644); err != nil {
		return "", err
	}
	f, err := os.Create(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := col.FlushNDJSON(f); err != nil {
		return "", err
	}
	return dir, nil
}
