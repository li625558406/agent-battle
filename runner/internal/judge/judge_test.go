package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentbattle/protocol"
)

const devKey = "dev-secret" // M1 开发密钥；平台化后由平台签发

// setupTask 构造已签名的判分任务目录 + 已提交基线的真实 git 沙箱。
// extra 追加额外判分命令（对应脚本须自行写入 testsDir）。
// 沙箱必须是真实 git 仓库并有基线 commit：否则 git diff HEAD 的致命错误
// 会与"无改动"不可区分，DiffHash 凭证被污染。
func setupTask(t *testing.T, extra ...protocol.TestCommand) (taskDir, sandbox string) {
	t.Helper()
	taskDir = t.TempDir()
	testsDir := filepath.Join(taskDir, "tests")
	if err := os.MkdirAll(testsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestCommand{
		{Name: "ok", Cmd: "bash .judge/run.sh"},
		{Name: "bad", Cmd: "bash .judge/fail.sh"},
	}
	tests = append(tests, extra...)
	mb, err := json.Marshal(protocol.JudgeManifest{TaskID: "t1", Tests: tests})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "run.sh"), []byte("exit 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "fail.sh"), []byte("exit 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "slow.sh"), []byte("sleep 5"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SignDir(taskDir, []byte(devKey)); err != nil {
		t.Fatal(err)
	}

	sandbox = t.TempDir()
	if err := os.WriteFile(filepath.Join(sandbox, "work.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sandbox, "init")
	gitRun(t, sandbox, "add", "-A")
	gitRun(t, sandbox, "-c", "core.autocrlf=false", "-c", "commit.gpgsign=false",
		"-c", "user.email=runner@agentbattle", "-c", "user.name=runner",
		"commit", "--allow-empty", "-m", "baseline")
	return taskDir, sandbox
}

// gitRun 在 dir 中执行 git 子命令，过滤 GIT_DIR/GIT_WORK_TREE 防误指宿主仓库
// （与 sandbox 包同款防御）。
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && (strings.EqualFold(k, "GIT_DIR") || strings.EqualFold(k, "GIT_WORK_TREE")) {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestRunVerifiesAndScores(t *testing.T) {
	taskDir, sb := setupTask(t)
	// 基线 commit 之后真实改动 work.txt，DiffHash 必须反映真实 diff，
	// 而不是退化为 sha256("") 的空串 hash。
	if err := os.WriteFile(filepath.Join(sb, "work.txt"), []byte("x\nchanged by agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(taskDir, sb, []byte(devKey))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 2 || rep.Passed != 1 {
		t.Fatalf("want 1/2, got %d/%d", rep.Passed, rep.Total)
	}
	if rep.Results[0].Name != "ok" || !rep.Results[0].Passed {
		t.Fatalf("bad first result: %+v", rep.Results[0])
	}
	emptySum := sha256.Sum256(nil)
	if rep.DiffHash == "" || rep.DiffHash == hex.EncodeToString(emptySum[:]) {
		t.Fatalf("diff hash must reflect real work.txt change, got %q", rep.DiffHash)
	}
}

// Important-1：沙箱不是 git 仓库时，git diff HEAD 的致命错误（退出码 128）
// 必须让 Run 失败，而不是静默产出 sha256("") 冒充"无改动"。
func TestRunFailsWhenSandboxNotGitRepo(t *testing.T) {
	taskDir, sb := setupTask(t)
	if err := os.RemoveAll(filepath.Join(sb, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(taskDir, sb, []byte(devKey)); err == nil {
		t.Fatal("non-git sandbox must fail Run, not produce fake empty diff hash")
	}
}

// Important-2：判分命令超时只影响该条结果（Passed=false、ExitCode=-1），
// 不得让 Run 整体报错——测试失败 ≠ 判分失败。
func TestRunCommandTimeout(t *testing.T) {
	taskDir, sb := setupTask(t, protocol.TestCommand{Name: "slow", Cmd: "bash .judge/slow.sh"})
	old := testTimeout
	testTimeout = 100 * time.Millisecond
	defer func() { testTimeout = old }()

	rep, err := Run(taskDir, sb, []byte(devKey))
	if err != nil {
		t.Fatalf("timeout in one test must not fail Run: %v", err)
	}
	if rep.Total != 3 || rep.Passed != 1 {
		t.Fatalf("want 1/3, got %d/%d", rep.Passed, rep.Total)
	}
	var slow *protocol.TestResult
	for i := range rep.Results {
		if rep.Results[i].Name == "slow" {
			slow = &rep.Results[i]
		}
	}
	if slow == nil {
		t.Fatal("slow result missing")
	}
	if slow.Passed {
		t.Fatalf("timed-out test reported as passed: %+v", slow)
	}
	if slow.ExitCode >= 0 {
		t.Fatalf("timed-out test should carry -1/non-zero exit code, got %d", slow.ExitCode)
	}
}

func TestTamperedManifestRejected(t *testing.T) {
	taskDir, sb := setupTask(t)
	mp := filepath.Join(taskDir, "tests", "manifest.json")
	if err := os.WriteFile(mp, []byte(`{"task_id":"t1","tests":[{"name":"evil","cmd":"echo pwned"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(taskDir, sb, []byte(devKey)); err == nil {
		t.Fatal("tampered judge package accepted")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	taskDir, sb := setupTask(t)
	if _, err := Run(taskDir, sb, []byte("wrong-key")); err == nil {
		t.Fatal("wrong key accepted")
	}
}

// M3：.gitignore 幂等检查以 ".judge/"（带斜杠）为准，重复 Run 不产生重复行。
func TestIgnoreJudgeDirIdempotent(t *testing.T) {
	sb := t.TempDir()
	if err := ignoreJudgeDir(sb); err != nil {
		t.Fatal(err)
	}
	if err := ignoreJudgeDir(sb); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sb, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != ".judge/\n" {
		t.Fatalf("want %q, got %q", ".judge/\n", got)
	}
}
