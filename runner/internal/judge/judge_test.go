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

// gitRev 返回 dir 仓库的 HEAD SHA（供 Run 的 baselineSHA 参数使用）。
func gitRev(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRunVerifiesAndScores(t *testing.T) {
	taskDir, sb := setupTask(t)
	// 基线 commit 之后真实改动 work.txt，DiffHash 必须反映真实 diff，
	// 而不是退化为 sha256("") 的空串 hash。
	if err := os.WriteFile(filepath.Join(sb, "work.txt"), []byte("x\nchanged by agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(taskDir, sb, gitRev(t, sb), []byte(devKey))
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
	baseline := gitRev(t, sb) // 先记录基线，再模拟 agent 删掉 .git
	if err := os.RemoveAll(filepath.Join(sb, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(taskDir, sb, baseline, []byte(devKey)); err == nil {
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

	rep, err := Run(taskDir, sb, gitRev(t, sb), []byte(devKey))
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
	if _, err := Run(taskDir, sb, gitRev(t, sb), []byte(devKey)); err == nil {
		t.Fatal("tampered judge package accepted")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	taskDir, sb := setupTask(t)
	if _, err := Run(taskDir, sb, gitRev(t, sb), []byte("wrong-key")); err == nil {
		t.Fatal("wrong key accepted")
	}
}

// baselineCmd 模拟 manifest 的基线校验命令：HEAD 必须等于注入的基线 SHA。
const baselineCmd = `[ "$(git rev-parse HEAD)" = "$AGENTBATTLE_BASELINE_SHA" ]`

// 基线 SHA 注入：诚实沙箱（历史未动）下，HEAD == AGENTBATTLE_BASELINE_SHA，
// 校验命令必须通过。
func TestRunInjectsBaselineSHA(t *testing.T) {
	taskDir, sb := setupTask(t, protocol.TestCommand{Name: "baseline-check", Cmd: baselineCmd})
	baseline := gitRev(t, sb)
	rep, err := Run(taskDir, sb, baseline, []byte(devKey))
	if err != nil {
		t.Fatal(err)
	}
	var check *protocol.TestResult
	for i := range rep.Results {
		if rep.Results[i].Name == "baseline-check" {
			check = &rep.Results[i]
		}
	}
	if check == nil || !check.Passed {
		t.Fatalf("baseline-check must pass on honest sandbox, got: %+v", check)
	}
}

// 对抗：agent 改动文件后 git commit --amend 根 commit——工作树干净、
// rev-list 计数仍为 1，但 HEAD 已不等于启动前记录的基线 → 校验必须 FAIL。
func TestRunDetectsAmendedHistory(t *testing.T) {
	taskDir, sb := setupTask(t, protocol.TestCommand{Name: "baseline-check", Cmd: baselineCmd})
	baseline := gitRev(t, sb)
	if err := os.WriteFile(filepath.Join(sb, "work.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sb, "-c", "user.email=runner@agentbattle", "-c", "user.name=runner",
		"commit", "--amend", "-a", "-m", "evil-amend")
	rep, err := Run(taskDir, sb, baseline, []byte(devKey))
	if err != nil {
		t.Fatal(err)
	}
	var check *protocol.TestResult
	for i := range rep.Results {
		if rep.Results[i].Name == "baseline-check" {
			check = &rep.Results[i]
		}
	}
	if check == nil {
		t.Fatal("baseline-check result missing")
	}
	if check.Passed {
		t.Fatalf("amended history must fail baseline-check, got: %+v", check)
	}
}

// baselineSHA 为空串时不注入环境变量（向后兼容），判分流程正常走完。
func TestRunEmptyBaselineNoInjection(t *testing.T) {
	taskDir, sb := setupTask(t, protocol.TestCommand{
		Name: "no-env", Cmd: `[ -z "$AGENTBATTLE_BASELINE_SHA" ]`})
	rep, err := Run(taskDir, sb, "", []byte(devKey))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		if r.Name == "no-env" && !r.Passed {
			t.Fatalf("empty baseline must not inject env var, got: %+v", r)
		}
	}
}

// 宿主环境恶意 GIT_DIR/GIT_WORK_TREE 不得影响 judge 内的 git 调用：
// hashGitDiff 的 git diff 与判分命令内的 git（baseline-check）都必须
// 操作沙箱仓库而非 hostile.git，否则 Run 报错 / 校验命令失败。
func TestRunIgnoresHostGitDirEnv(t *testing.T) {
	taskDir, sb := setupTask(t, protocol.TestCommand{Name: "baseline-check", Cmd: baselineCmd})
	hostile := filepath.Join(t.TempDir(), "hostile.git")
	baseline := gitRev(t, sb)
	t.Setenv("GIT_DIR", hostile)
	t.Setenv("GIT_WORK_TREE", ".")
	rep, err := Run(taskDir, sb, baseline, []byte(devKey))
	if err != nil {
		t.Fatalf("host GIT_DIR leaked into judge git calls: %v", err)
	}
	if rep.Total == 0 {
		t.Fatal("expected tests to run")
	}
	var check *protocol.TestResult
	for i := range rep.Results {
		if rep.Results[i].Name == "baseline-check" {
			check = &rep.Results[i]
		}
	}
	if check == nil || !check.Passed {
		t.Fatalf("baseline-check must pass with hostile GIT_DIR filtered, got: %+v", check)
	}
}

// VerifyDir 对合法签名目录必须放行。
func TestVerifyDirAcceptsValidDir(t *testing.T) {
	taskDir, _ := setupTask(t)
	if err := VerifyDir(taskDir, []byte(devKey)); err != nil {
		t.Fatalf("valid dir rejected: %v", err)
	}
}

// 对抗：manifest 内容被篡改后 VerifyDir 必须拒绝。
func TestVerifyDirRejectsTamperedManifest(t *testing.T) {
	taskDir, _ := setupTask(t)
	mPath := filepath.Join(taskDir, "tests", "manifest.json")
	b, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}
	// 追加空白不会改变 canonical 形态（验签设计如此），这里改真实内容。
	tampered := strings.Replace(string(b), `"t1"`, `"t9"`, 1)
	if tampered == string(b) {
		t.Fatal("tamper payload did not modify manifest")
	}
	if err := os.WriteFile(mPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDir(taskDir, []byte(devKey)); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

// 对抗：manifest 或 sig 缺失时 VerifyDir 必须带原因拒绝。
func TestVerifyDirRejectsMissingPieces(t *testing.T) {
	taskDir, _ := setupTask(t)
	if err := os.Remove(filepath.Join(taskDir, "tests", "sig")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDir(taskDir, []byte(devKey)); err == nil {
		t.Fatal("missing sig accepted")
	}
	if err := os.Remove(filepath.Join(taskDir, "tests", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDir(taskDir, []byte(devKey)); err == nil {
		t.Fatal("missing manifest accepted")
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
