package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseLineToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"x.py","old_string":"a","new_string":"b"}}]}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events (tool_call + file_edit), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != "tool_call" || evs[0].Tool != "Edit" {
		t.Fatalf("bad ev0: %+v", evs[0])
	}
	if evs[0].ArgsHash == "" || strings.Contains(evs[0].ArgsHash, "old_string") {
		t.Fatal("ArgsHash must be a hash, never raw args")
	}
	if evs[1].Type != "file_edit" || evs[1].Note != "x.py" {
		t.Fatalf("bad ev1: %+v", evs[1])
	}
}

func TestParseLineResult(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":4200,"num_turns":3,"usage":{"input_tokens":100,"output_tokens":50}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "result" || evs[0].DurationMS != 4200 || evs[0].Tokens != 150 {
		t.Fatalf("bad: %+v err=%v", evs, err)
	}
}

func TestParseLineIgnorable(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`not json at all`,
		``,
	} {
		evs, err := parseLine([]byte(line))
		if err != nil {
			t.Fatalf("line %q: unexpected error %v", line, err)
		}
		if len(evs) > 1 {
			t.Fatalf("line %q: too many events %v", line, evs)
		}
	}
}

func TestParseLineMultipartContent(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Tool != "Bash" {
		t.Fatalf("bad: %+v", evs)
	}
}

func TestParseLineResultIsError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"duration_ms":100,"usage":{"input_tokens":1,"output_tokens":1}}`
	evs, err := parseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "error" {
		t.Fatalf("bad: %+v", evs)
	}
}

// requireBash 本组 Launch 测试依赖 git-bash 执行 fixture 脚本。
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用，跳过 Launch 集成测试")
	}
}

// writeFixtureScript 写入一个由 bash 执行的假 claude 脚本，返回其绝对路径。
func writeFixtureScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// hookWithScript 返回把 cmd 构建替换为 `bash <script>` 的测试钩子。
func hookWithScript(script string) func(bin string, args []string) *exec.Cmd {
	return func(bin string, args []string) *exec.Cmd {
		return exec.Command("bash", script)
	}
}

func TestLaunchStreamsEvents(t *testing.T) {
	requireBash(t)
	script := writeFixtureScript(t, `
echo '{"type":"system","subtype":"init"}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}'
echo '{"type":"result","duration_ms":100,"is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
sleep 0.1
exit 0
`)
	cc := ClaudeCode{testCmdHook: hookWithScript(script)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := make(chan RawEvent, 16)
	if err := cc.Launch(ctx, t.TempDir(), "task", nil, out); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	close(out)
	var sawToolCall, sawResult bool
	for ev := range out {
		switch {
		case ev.Type == "tool_call" && ev.Tool == "Bash":
			sawToolCall = true
		case ev.Type == "result" && ev.DurationMS == 100 && ev.Tokens == 15:
			sawResult = true
		}
	}
	if !sawToolCall || !sawResult {
		t.Fatalf("missing events: tool_call=%v result=%v", sawToolCall, sawResult)
	}
}

func TestLaunchStderrTailOnFailure(t *testing.T) {
	requireBash(t)
	script := writeFixtureScript(t, `
echo "FATAL_MARKER_stderr_probe" >&2
exit 3
`)
	cc := ClaudeCode{testCmdHook: hookWithScript(script)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := make(chan RawEvent, 16)
	err := cc.Launch(ctx, t.TempDir(), "task", nil, out)
	if err == nil {
		t.Fatal("want error on nonzero exit")
	}
	if !strings.Contains(err.Error(), "FATAL_MARKER_stderr_probe") {
		t.Fatalf("error missing stderr tail: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error missing exit status: %v", err)
	}
}

// TestLaunchOverlongLine >16MB 无换行单行 + 之后合法 result 行：不挂死、
// 产出超长行 error 事件、result 事件仍被采集、返回 nil。
func TestLaunchOverlongLine(t *testing.T) {
	requireBash(t)
	script := writeFixtureScript(t, `
head -c 17000000 /dev/zero | tr '\0' 'a'
echo
echo '{"type":"result","duration_ms":50,"is_error":false,"usage":{"input_tokens":1,"output_tokens":2}}'
exit 0
`)
	cc := ClaudeCode{testCmdHook: hookWithScript(script)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // 挂死兜底
	defer cancel()
	out := make(chan RawEvent, 16)
	if err := cc.Launch(ctx, t.TempDir(), "task", nil, out); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	close(out)
	var sawTooLongErr, sawResult bool
	for ev := range out {
		switch {
		case ev.Type == "error" && strings.Contains(ev.Note, "超长行"):
			sawTooLongErr = true
		case ev.Type == "result" && ev.DurationMS == 50 && ev.Tokens == 3:
			sawResult = true
		}
	}
	if !sawTooLongErr {
		t.Fatal("missing overlong-line error event")
	}
	if !sawResult {
		t.Fatal("missing result event after overlong line")
	}
}

func TestTruncateTailRuneSafe(t *testing.T) {
	// 中文字符 3 字节：按字节切到 2000 会把多字节字符拦腰截断
	s := strings.Repeat("汉", 700) + strings.Repeat("b", 500)
	got := truncateTail(s, 2000)
	if len(got) > 2000 {
		t.Fatalf("len = %d, want <= 2000", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid utf8: %q", got[:30])
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 500)) {
		t.Fatal("tail bytes lost")
	}
	// 短于 max 原样返回
	if truncateTail("abc", 2000) != "abc" {
		t.Fatal("short input must be unchanged")
	}
}
