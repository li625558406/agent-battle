// Package judge 负责验证判分包签名并在沙箱内执行测试、产出 JudgeReport。
//
// 判分包防篡改模型：manifest.json 经 HMAC-SHA256(canonicalJSON) 签名后
// 写入 tests/sig；验证时对原文重新做 canonical 序列化再比对，
// 空白差异不影响验签，任何内容改动（含错 key 签的包）都会被拒绝。
package judge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"agentbattle/protocol"
)

const (
	manifestName = "manifest.json"
	sigName      = "sig"
	judgeDirName = ".judge"
	// judgeIgnoreLine 是写入沙箱 .gitignore 的统一条目（带斜杠，仅匹配目录）。
	judgeIgnoreLine = ".judge/"
)

// SignDir 对 taskDir/tests/manifest.json 做 HMAC-SHA256 签名，
// hex 编码后写入 taskDir/tests/sig。
//
// 先 canonical 化（解析后重新序列化）再签名，消除空白/格式差异，
// 保证跨机器（不同编辑器格式化习惯）可验证。
// 注意：未知字段不在签名覆盖范围内——解析进 Go 结构体时即被丢弃，
// 不会参与 canonical 序列化，因此追加未知字段的篡改不会被验签发现
// （已知字段的任何改动仍会被拒绝）。
func SignDir(taskDir string, key []byte) error {
	manifestPath := filepath.Join(taskDir, "tests", manifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("judge: read manifest: %w", err)
	}
	var m protocol.JudgeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("judge: manifest 不是合法 JSON: %w", err)
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("judge: canonical 序列化失败: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	sig := hex.EncodeToString(mac.Sum(nil))
	if err := os.WriteFile(filepath.Join(taskDir, "tests", sigName), []byte(sig), 0o644); err != nil {
		return fmt.Errorf("judge: write sig: %w", err)
	}
	return nil
}

// verifyManifest 读取 manifest 与 sig，HMAC 恒时比较。
// sig 缺失、hex 解码失败、内容或 key 不匹配一律拒绝。
// 同 SignDir：canonical 化基于结构体序列化，未知字段不在签名覆盖范围内。
func verifyManifest(testsDir string, key []byte) (protocol.JudgeManifest, error) {
	var m protocol.JudgeManifest
	raw, err := os.ReadFile(filepath.Join(testsDir, manifestName))
	if err != nil {
		return m, fmt.Errorf("judge: read manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("judge: manifest 不是合法 JSON: %w", err)
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return m, fmt.Errorf("judge: canonical 序列化失败: %w", err)
	}
	sigHex, err := os.ReadFile(filepath.Join(testsDir, sigName))
	if err != nil {
		return m, fmt.Errorf("judge: read sig: %w", err)
	}
	want, err := hex.DecodeString(string(bytes.TrimSpace(sigHex)))
	if err != nil {
		return m, fmt.Errorf("judge: sig 不是合法 hex: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	if !hmac.Equal(mac.Sum(nil), want) {
		return m, fmt.Errorf("judge: 判分包验签失败（内容被篡改或 key 错误）")
	}
	return m, nil
}

// Run 验签判分包后，在沙箱内逐条执行测试并产出 JudgeReport。
//
// baselineSHA 是 agent 启动前记录的沙箱 HEAD SHA（见 sandbox.HeadRev），
// 会以环境变量 AGENTBATTLE_BASELINE_SHA 注入每条判分命令，供 manifest 侧
// 校验 agent 未改写提交历史（如 commit --amend 架空工作树 diff 校验）。
// 传空串则不注入（兼容无基线校验的判分包）。
//
// 步骤：验签 → 拷贝 tests/（除 manifest.json、sig）到 sandbox/.judge/ →
// 沙箱 .gitignore 追加 .judge/ → bash -c 执行 → git diff HEAD 计算 DiffHash。
func Run(taskDir, sandbox, baselineSHA string, key []byte) (protocol.JudgeReport, error) {
	testsDir := filepath.Join(taskDir, "tests")
	m, err := verifyManifest(testsDir, key)
	if err != nil {
		return protocol.JudgeReport{}, err
	}

	judgeDir := filepath.Join(sandbox, judgeDirName)
	if err := copyTree(testsDir, judgeDir); err != nil {
		return protocol.JudgeReport{}, fmt.Errorf("judge: 拷贝判分脚本失败: %w", err)
	}
	if err := ignoreJudgeDir(sandbox); err != nil {
		return protocol.JudgeReport{}, fmt.Errorf("judge: 更新 .gitignore 失败: %w", err)
	}

	rep := protocol.JudgeReport{TaskID: m.TaskID}
	for _, tc := range m.Tests {
		res := runOne(tc, sandbox, baselineSHA)
		rep.Results = append(rep.Results, res)
		if res.Passed {
			rep.Passed++
		}
	}
	rep.Total = len(m.Tests)

	diffHash, err := hashGitDiff(sandbox)
	if err != nil {
		return protocol.JudgeReport{}, fmt.Errorf("judge: git diff: %w", err)
	}
	rep.DiffHash = diffHash
	return rep, nil
}

// testTimeout 是单条判分命令的硬超时。
// 包级 var（非 const）以便测试注入更短超时做对抗验证。
var testTimeout = 2 * time.Minute

// runOne 在沙箱 cwd 下执行单条测试命令。
// baselineSHA 非空时以 AGENTBATTLE_BASELINE_SHA 注入命令环境（env 由 judge
// 进程构造，agent 进程已退出、无法影响）；空串则不注入。
// 命令超时（testTimeout）被杀 → Passed=false、ExitCode=-1，
// 错误不外抛：测试失败 ≠ 判分失败。
func runOne(tc protocol.TestCommand, sandbox, baselineSHA string) protocol.TestResult {
	res := protocol.TestResult{Name: tc.Name}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", tc.Cmd)
	cmd.Dir = sandbox
	if baselineSHA != "" {
		cmd.Env = append(os.Environ(), "AGENTBATTLE_BASELINE_SHA="+baselineSHA)
	}
	// 超时 kill 只杀 bash 自身；其孤儿子进程可能继承 stdout 管道导致
	// Wait 永久阻塞（如 sleep infinity）。WaitDelay 保证 kill 后最迟
	// 2s 强制关闭管道返回，超时是真正的硬上界。
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		// 超时被杀：标记该条失败，ExitCode=-1，但不让判分流程报错。
		res.Passed = false
		res.ExitCode = -1
	} else {
		res.Passed = err == nil
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else if err != nil {
			res.ExitCode = -1 // 启动失败等非退出码错误
		}
	}
	sum := sha256.Sum256(out)
	res.LogHash = hex.EncodeToString(sum[:])
	return res
}

// hashGitDiff 计算沙箱内 git diff HEAD 输出的 sha256。
// diff 输出走 stdout。任何 ExitError 都视为致命错误（退出码 128 通常意味着
// 沙箱不是 git 仓库或无基线 commit）——判分凭证宁可让判分失败，
// 也绝不静默退化为 sha256("") 而与"无改动"的合法 hash 混淆。
// 返回的 error 附带 stdout 摘要便于排查。
func hashGitDiff(sandbox string) (string, error) {
	cmd := exec.Command("git", "diff", "HEAD")
	cmd.Dir = sandbox
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff HEAD 退出码 %d（沙箱可能不是 git 仓库或无基线 commit）: %.200s",
				ee.ExitCode(), stdout.String())
		}
		return "", fmt.Errorf("run git diff: %w", err)
	}
	sum := sha256.Sum256(stdout.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// ignoreJudgeDir 向沙箱 .gitignore 追加一行 ".judge/"（不存在则创建）。
// 已包含该行则不重复追加。
func ignoreJudgeDir(sandbox string) error {
	p := filepath.Join(sandbox, ".gitignore")
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if string(bytes.TrimSpace(line)) == judgeIgnoreLine {
			return nil
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.Write([]byte("\n")); err != nil {
			return err
		}
	}
	_, err = f.WriteString(judgeIgnoreLine + "\n")
	return err
}

// copyTree 递归拷贝 src 目录到 dst（跳过 manifest.json 与 sig）。
// 覆盖语义：dst 中已存在的同名文件/目录会被直接覆盖；dst 中 src 没有的
// 旧文件会残留（本函数不做清理）；所有文件统一按 0o755 写入，保证判分脚本
// 可直接执行；符号链接会被解引用，拷贝的是链接指向的文件内容。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755) // dst 根目录自身也要创建
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if rel == manifestName || rel == sigName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o755)
	})
}
