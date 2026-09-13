package adapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ClaudeCode 接入 Claude Code headless（claude -p --output-format stream-json）。
type ClaudeCode struct {
	Bin  string // 为空则用 "claude"
	YOLO bool   // true 时追加 --dangerously-skip-permissions
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
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claude-code: stdout pipe: %w", err)
	}
	var stderrTail string // 尾部 2000 字符，供 cmd.Wait 出错时返回
	cmd.Stderr = io.MultiWriter(stderrWriter{func(p []byte) {
		s := string(p)
		stderrTail += s
		if len(stderrTail) > 2000 {
			stderrTail = stderrTail[len(stderrTail)-2000:]
		}
	}})

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude-code: start: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 容忍超长行
	for scanner.Scan() {
		evs, _ := parseLine(scanner.Bytes())
		for _, ev := range evs {
			select {
			case out <- ev:
			case <-ctx.Done():
				_ = cmd.Wait() // ctx 取消已杀进程，仅回收
				return ctx.Err()
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude-code: exited: %w; stderr tail: %s", err, stderrTail)
	}
	return scanner.Err()
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
