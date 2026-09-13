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

	col := collector.New()
	events := make(chan adapter.RawEvent, 256)
	execDone := make(chan error, 1)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	go func() {
		execDone <- cfg.Adapter.Launch(runCtx, sb, task.Description, cfg.Env, events)
	}()

agentLoop:
	for {
		select {
		case raw, ok := <-events:
			if !ok {
				break agentLoop
			}
			col.Add(raw)
		case err := <-execDone:
			if err != nil {
				col.Add(adapter.RawEvent{Type: protocol.EventError, Note: "agent 异常退出"})
				return Result{}, fmt.Errorf("agent 执行失败: %w", err)
			}
			// 渠道里可能还有缓冲事件，排干
			for {
				select {
				case raw, ok := <-events:
					if !ok {
						break agentLoop
					}
					col.Add(raw)
				default:
					break agentLoop
				}
			}
		}
	}
	wall := time.Since(start).Milliseconds()

	rep, err := judge.Run(cfg.TaskDir, sb, cfg.JudgeKey)
	if err != nil {
		return Result{}, fmt.Errorf("判分失败: %w", err)
	}

	dir, err := writeReport(cfg, task, rep, col, wall)
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

func writeReport(cfg Config, task protocol.TaskManifest, rep protocol.JudgeReport,
	col *collector.Collector, wallMS int64) (string, error) {
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
