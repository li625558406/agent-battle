package e2e

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPlatformLoopEcho 平台黑盒全链路：起 server 进程 → CLI 二进制注册
// A/B → fetch 任务包 → mirror --server（echo agent，1 轮）→ 断言结算与
// 天梯可见。
//
// 因 Go internal 边界（platform 不能 import runner/internal），本测试不做
// 进程内组装，全部通过 exec 真实二进制完成——CLI 输出格式（register 的
// "token: " 行、mirror 的结算行、ladder 表格）本身即被测契约。
//
// 确定性说明：--fix-a 给 A 侧注入正确解法（判 2/2），B 侧空配置
//（file-only-change 判分对未改动工作区 vacuously 通过，判 1/2），
// 通过比例决胜 → winner=a 恒定，与 WallMS 抖动无关。断言覆盖：无崩溃、
// 本地汇总行胜负平（runner 侧 session.winner 判 a 胜）、定级赛结算分
//（1220/1180）、天梯可见且统计四列与结算交叉一致——本地与平台两侧
// winner 断言合起来防 runner 侧 winner 与平台侧 winnerOf 两套规则漂移。
func TestPlatformLoopEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过黑盒 E2E")
	}
	root := findRepoRoot(t)

	// 构建 CLI 二进制：避免 go run 的编译输出混入 stdout 解析
	bin := filepath.Join(t.TempDir(), "agentbattle.exe")
	build := func(target, pkg string) {
		t.Helper()
		c := exec.Command("go", "build", "-o", target, pkg)
		c.Dir = root // 包相对路径以仓库根为基准
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("构建 %s 失败: %v\n%s", pkg, err, out)
		}
	}
	build(bin, "./runner/cmd/agentbattle")

	// server 二进制（不用 go run：Windows 下 go run 的子进程 kill 不干净）
	serverBin := filepath.Join(t.TempDir(), "agentbattle-server.exe")
	build(serverBin, "./platform/cmd/agentbattle-server")

	// server：独立任务目录（从 examples/fix-add 纯 Go 递归拷贝，不依赖外部 cp）
	tasksDir := t.TempDir()
	if err := copyDir(filepath.Join(root, "examples", "fix-add"), filepath.Join(tasksDir, "fix-add")); err != nil {
		t.Fatalf("拷贝示例任务失败: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	srvCmd := exec.Command(serverBin, "--addr", addr, "--tasks", tasksDir, "--store", dbPath)
	srvCmd.Dir = root
	var srvOut bytes.Buffer
	srvCmd.Stdout = &srvOut
	srvCmd.Stderr = &srvOut
	if err := srvCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srvCmd.Process.Kill()
		srvCmd.Wait()
		if t.Failed() {
			t.Logf("server 输出:\n%s", srvOut.String())
		}
	})
	waitHTTP(t, "http://"+addr+"/api/ladder")

	// run 执行 CLI 子命令，CombinedOutput 便于错误时完整回显
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, b)
		}
		return string(b)
	}

	// 注册 A/B（token 只出现在注册响应，从 "token: " 行解析）
	tokA := extractToken(t, run("register", "--server", "http://"+addr, "--name", "echoA"))
	tokB := extractToken(t, run("register", "--server", "http://"+addr, "--name", "echoB"))

	// fetch 任务包：zip 内条目以平台 tasksDir/fix-add 为根（见
	// platform/internal/api handleBundle），解压后 taskOut/task.json，
	// 故 mirror 的 --task 取 taskOut 本身而非 taskOut/fix-add。
	taskOut := filepath.Join(t.TempDir(), "task")
	run("fetch", "--server", "http://"+addr, "--task", "fix-add", "--out", taskOut)
	if _, err := os.Stat(filepath.Join(taskOut, "task.json")); err != nil {
		t.Fatalf("任务包解压后缺 task.json: %v", err)
	}

	// mirror --server 1 轮：任务包源自 server（tests/sig 用默认 dev-secret
	// 签名），judge 预检（VerifyDir）应通过。
	// --fix-a 的解法串与 examples/fix-add 判分命令强耦合（calc.sh 的 add()
	// 需输出两数之和）；任务判分口径变更时此处必须同步修改。
	rep := run("mirror",
		"--server", "http://"+addr,
		"--task", taskOut,
		"--task-id", "fix-add",
		"--agent", "echo",
		"--fix-a", "add() { echo $(( $1 + $2 )); }",
		"--rounds", "1",
		"--name-a", "echoA", "--token-a", tokA,
		"--name-b", "echoB", "--token-b", tokB,
		"--out", filepath.Join(t.TempDir(), "reports"))

	// 汇总行：无任何一侧崩溃；本地胜负平与预期一致——该行由 runner 侧
	// session.winner 判定（与平台侧 winnerOf 是两套独立实现），此处断言
	// 与下方结算/天梯断言合起来防两套 winner 规则漂移
	if !strings.Contains(rep, "镜像对战完成") {
		t.Fatalf("mirror 输出缺汇总行:\n%s", rep)
	}
	if !strings.Contains(rep, "A崩 0 | B崩 0") {
		t.Fatalf("mirror 存在崩溃侧:\n%s", rep)
	}
	if !strings.Contains(rep, "A 胜 1 | B 胜 0 | 平 0") {
		t.Fatalf("本地汇总行胜负平不符（runner 侧 winner 应判 a 胜）:\n%s", rep)
	}

	// 结算行：A 修复（2/2）、B 不修复（0/2）→ winner=a 确定性；
	// 定级赛 K=40 → A 1220 / B 1180
	settleRe := regexp.MustCompile(`第 1 轮 结算 winner=(\S+) A分=(\d+) B分=(\d+)`)
	m := settleRe.FindStringSubmatch(rep)
	if m == nil {
		t.Fatalf("mirror 输出缺结算行:\n%s", rep)
	}
	winner, ratingA, ratingB := m[1], m[2], m[3]
	const (
		wantA      = "1220"
		wantB      = "1180"
		wantStatA  = "1 0 0" // 胜 负 平
		wantStatB  = "0 1 0"
		winnerName = "echoA"
		loserName  = "echoB"
	)
	if winner != "a" {
		t.Fatalf("注入 --fix-a 后 winner 应确定为 a，got %q\n%s", winner, rep)
	}
	if ratingA != wantA || ratingB != wantB {
		t.Fatalf("定级赛结算分错误: A分=%s B分=%s (期望 %s/%s)\n%s", ratingA, ratingB, wantA, wantB, rep)
	}

	// 天梯可见：双方在榜，且统计三列（胜 负 平）与结算交叉一致（局部视角
	// 防两套 winner 判定规则——runner 侧 winner 与平台侧 winnerOf——漂移）
	lad := run("ladder", "--server", "http://"+addr)
	rows := parseLadder(t, lad)
	rowWin, ok := rows[winnerName]
	if !ok {
		t.Fatalf("天梯缺 %s:\n%s", winnerName, lad)
	}
	rowLose, ok := rows[loserName]
	if !ok {
		t.Fatalf("天梯缺 %s:\n%s", loserName, lad)
	}
	gotStatA := fmt.Sprintf("%d %d %d", rowWin.wins, rowWin.losses, rowWin.ties)
	gotStatB := fmt.Sprintf("%d %d %d", rowLose.wins, rowLose.losses, rowLose.ties)
	wantAi, _ := strconv.Atoi(wantA)
	wantBi, _ := strconv.Atoi(wantB)
	if rowWin.rating != wantAi || rowLose.rating != wantBi {
		t.Fatalf("天梯评分与结算不一致: %s=%v %s=%v\n%s", winnerName, rowWin, loserName, rowLose, lad)
	}
	if rowWin.games != 1 || rowLose.games != 1 {
		t.Fatalf("天梯局数错误: %s games=%d %s games=%d\n%s", winnerName, rowWin.games, loserName, rowLose.games, lad)
	}
	if gotStatA != wantStatA || gotStatB != wantStatB {
		t.Fatalf("天梯统计列与结算不一致: %s=[%s] %s=[%s] (期望 [%s] [%s])\n%s",
			winnerName, gotStatA, loserName, gotStatB, wantStatA, wantStatB, lad)
	}
}

// ladderRow 是天梯表格一行的解析结果（名称 评分 局数 胜 负 平）。
type ladderRow struct {
	rating                    int
	games, wins, losses, ties int
}

// parseLadder 解析 ladder 子命令输出的表格，返回 名称 → 行 的映射。
// 表头行与非表格行自动跳过。
func parseLadder(t *testing.T, out string) map[string]ladderRow {
	t.Helper()
	rows := map[string]ladderRow{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 6 {
			continue
		}
		r, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		g, e1 := strconv.Atoi(f[2])
		w, e2 := strconv.Atoi(f[3])
		l, e3 := strconv.Atoi(f[4])
		ti, e4 := strconv.Atoi(f[5])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		rows[f[0]] = ladderRow{rating: r, games: g, wins: w, losses: l, ties: ti}
	}
	return rows
}

// extractToken 从 register 输出解析 "token: <tok>" 行。
func extractToken(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if tok, ok := strings.CutPrefix(line, "token: "); ok {
			return strings.TrimSpace(tok)
		}
	}
	t.Fatalf("register 输出缺 token 行:\n%s", out)
	return ""
}

// waitHTTP 轮询直到 url 返回 200，30s 超时（留足 server 冷启动余量）。
func waitHTTP(t *testing.T, url string) {
	t.Helper()
	// 带单次请求超时：防 server 半就绪（bind 成功但 handler 挂死）时
	// 单次 Get 无限阻塞使 30s deadline 失效。
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server 30s 内未就绪: %s", url)
}

// freePort 申请一个空闲端口（先 Listen 再关闭，存在极小的竞占窗口，
// 测试场景可接受）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// copyDir 纯 Go 递归拷贝目录（普通文件与子目录；不跟随符号链接）。
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		outF, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(outF, in); err != nil {
			outF.Close()
			return err
		}
		return outF.Close()
	})
}
