package adapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"unicode/utf8"
)

// maxLineBytes 单行 stream-json 的容忍上限（16MB）。
const maxLineBytes = 16 * 1024 * 1024

// stderrTailMax 保留在错误信息里的 stderr 尾部长度（字节）。
const stderrTailMax = 2000

// ClaudeCode 接入 Claude Code headless（claude -p --output-format stream-json）。
type ClaudeCode struct {
	Bin  string // 为空则用 "claude"
	YOLO bool   // true 时追加 --dangerously-skip-permissions

	// testCmdHook 仅测试注入用：非 nil 时替代 exec.CommandContext 构建 cmd。
	testCmdHook func(bin string, args []string) *exec.Cmd
}

func (c ClaudeCode) bin() string {
	if c.Bin == "" {
		return "claude"
	}
	return c.Bin
}

func (c ClaudeCode) Name() string { return "claude-code" }

func (c ClaudeCode) Detect() error {
	_, err := exec.LookPath(c.bin())
	return err
}

// streamJSONLine 是 stream-json 输出中我们关心的字段（未知字段一律忽略）。
type streamJSONLine struct {
	Type string `json:"type"`
	// type == "assistant" 时有效
	Message struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`
	// type == "result" 时有效
	DurationMS int64 `json:"duration_ms"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	IsError bool `json:"is_error"`
}

type contentBlock struct {
	Type  string          `json:"type"` // "text" / "tool_use" / ...
	Name  string          `json:"name"` // tool_use: 工具名
	Input json.RawMessage `json:"input"`
}

func (c ClaudeCode) Launch(ctx context.Context, cwd, taskDescription string, env []string, out chan<- RawEvent) error {
	args := []string{"-p", taskDescription, "--output-format", "stream-json", "--verbose"}
	if c.YOLO {
		args = append(args, "--dangerously-skip-permissions")
	}
	var cmd *exec.Cmd
	if c.testCmdHook != nil {
		cmd = c.testCmdHook(c.bin(), args)
	} else {
		cmd = exec.CommandContext(ctx, c.bin(), args...)
	}
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claude-code: stdout pipe: %w", err)
	}
	var stderrTail string // 尾部 2000 字节（rune 边界安全），供 cmd.Wait 出错时返回
	cmd.Stderr = io.MultiWriter(stderrWriter{func(p []byte) {
		stderrTail = truncateTail(stderrTail+string(p), stderrTailMax)
	}})

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude-code: start: %w", err)
	}

	scanErr := c.pump(ctx, stdout, out)
	// scanner 退出后（无论原因）先排空管道残余再 Wait：
	// 否则子进程写满管道会阻塞到永远，Wait 随之挂死且掩盖真实错误。
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	switch {
	case waitErr != nil && scanErr != nil:
		return fmt.Errorf("claude-code: exited: %w; stdout scan: %v; stderr tail: %s", waitErr, scanErr, stderrTail)
	case waitErr != nil:
		return fmt.Errorf("claude-code: exited: %w; stderr tail: %s", waitErr, stderrTail)
	case scanErr != nil:
		return fmt.Errorf("claude-code: stdout scan: %w", scanErr)
	}
	return nil
}

// pump 逐行扫描 stdout 并把事件发到 out，返回 scanner 终止时的错误。
// 遇到超过 maxLineBytes 的超长行：产出 error 事件、丢弃该行剩余部分后继续解析后续行。
func (c ClaudeCode) pump(ctx context.Context, stdout io.Reader, out chan<- RawEvent) error {
	rest := io.Reader(stdout)
	for {
		scanner := bufio.NewScanner(rest)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes) // 容忍超长行
		for scanner.Scan() {
			evs, _ := parseLine(scanner.Bytes())
			if !sendAll(ctx, out, evs) {
				return ctx.Err()
			}
		}
		err := scanner.Err()
		if err == nil {
			return nil
		}
		if !errors.Is(err, bufio.ErrTooLong) {
			return err
		}
		// 超长行：报告后跳到行尾，继续解析后续行（result 行仍在后面）
		if !sendAll(ctx, out, []RawEvent{{Type: "error", Note: "stdout 超长行（>16MB）被截断丢弃"}}) {
			return ctx.Err()
		}
		rest = discardToNewline(rest)
	}
}

// sendAll 逐个发送事件；ctx 取消或超时时返回 false。
func sendAll(ctx context.Context, out chan<- RawEvent, evs []RawEvent) bool {
	for _, ev := range evs {
		select {
		case out <- ev:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// discardToNewline 读取并丢弃 r 中直到下一个 '\n'（含）的字节。
// 返回的 reader 缓冲保留了换行之后的数据，可直接继续扫描后续行。
func discardToNewline(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	_, _ = br.ReadBytes('\n') // EOF 时同样安全：缓冲为空，后续 scanner 直接结束
	return br
}

// truncateTail 保留 s 尾部至多 max 字节，起点推进到 rune 边界，避免切断多字节 UTF-8 字符。
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// stderrWriter 把回调适配成 io.Writer。
type stderrWriter struct{ fn func([]byte) }

func (w stderrWriter) Write(p []byte) (int, error) {
	w.fn(p)
	return len(p), nil
}

// parseLine 解析 stream-json 的一行。
// 空行 / 可忽略行（system/init 等）返回 nil；非法 JSON 行降级为 1 个 error 事件而非报错（不中断流）。
// error 返回值恒为 nil：解析失败不中断整个流。
func parseLine(line []byte) ([]RawEvent, error) {
	if len(line) == 0 {
		return nil, nil
	}
	var l streamJSONLine
	if err := json.Unmarshal(line, &l); err != nil {
		return []RawEvent{{Type: "error", Note: "stdout 非 JSON 行"}}, nil
	}
	switch l.Type {
	case "assistant":
		var evs []RawEvent
		for _, blk := range l.Message.Content {
			if blk.Type != "tool_use" {
				continue
			}
			hash := sha256.Sum256(blk.Input) // 只存 hash，绝不放原始参数
			evs = append(evs, RawEvent{Type: "tool_call", Tool: blk.Name, ArgsHash: hex.EncodeToString(hash[:])})
			if blk.Name == "Edit" || blk.Name == "Write" || blk.Name == "MultiEdit" {
				evs = append(evs, RawEvent{Type: "file_edit", Tool: blk.Name, Note: extractFilePath(blk.Input)})
			}
		}
		return evs, nil
	case "result":
		t := "result"
		if l.IsError {
			t = "error"
		}
		return []RawEvent{{
			Type:       t,
			DurationMS: l.DurationMS,
			Tokens:     l.Usage.InputTokens + l.Usage.OutputTokens,
		}}, nil
	default: // system / user / stream_event 等一律忽略
		return nil, nil
	}
}

// extractFilePath 尽力从工具 input 中取 file_path，失败返回空串。
func extractFilePath(input json.RawMessage) string {
	var m struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	return m.FilePath
}
