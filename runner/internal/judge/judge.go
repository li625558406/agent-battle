// Package judge 负责验证判分包签名并在沙箱内执行测试、产出 JudgeReport。
//
// 判分包防篡改模型：manifest.json 经 HMAC-SHA256(canonicalJSON) 签名后
// 写入 tests/sig；验证时对原文重新做 canonical 序列化再比对，
// 空白差异不影响验签，任何内容改动（含错 key 签的包）都会被拒绝。
package judge

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"agentbattle/protocol"
)

const (
	manifestName = "manifest.json"
	sigName      = "sig"
	judgeDirName = ".judge"
)

// SignDir 对 taskDir/tests/manifest.json 做 HMAC-SHA256 签名，
// hex 编码后写入 taskDir/tests/sig。
//
// 先 canonical 化（解析后重新序列化）再签名，消除空白/格式差异，
// 保证跨机器（不同编辑器格式化习惯）可验证。
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
// 步骤：验签 → 拷贝 tests/（除 manifest.json、sig）到 sandbox/.judge/ →
// 沙箱 .gitignore 追加 .judge/ → bash -c 执行 → git diff HEAD 计算 DiffHash。
func Run(taskDir, sandbox string, key []byte) (protocol.JudgeReport, error) {
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
		res := runOne(tc, sandbox)
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

// runOne 在沙箱 cwd 下执行单条测试命令。
func runOne(tc protocol.TestCommand, sandbox string) protocol.TestResult {
	res := protocol.TestResult{Name: tc.Name}
	cmd := exec.Command("bash", "-c", tc.Cmd)
	cmd.Dir = sandbox
	out, err := cmd.CombinedOutput()
	res.Passed = err == nil
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if err != nil {
		res.ExitCode = -1 // 启动失败等非退出码错误
	}
	sum := sha256.Sum256(out)
	res.LogHash = hex.EncodeToString(sum[:])
	return res
}

// hashGitDiff 计算沙箱内 git diff HEAD 输出的 sha256。
// diff 输出走 stdout；ExitError（含有输出的 diff 失败）不视为致命，
// 因为输出本身已可参与 hash。此处仅对命令启动失败返回 error。
func hashGitDiff(sandbox string) (string, error) {
	cmd := exec.Command("git", "diff", "HEAD")
	cmd.Dir = sandbox
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return "", fmt.Errorf("run git diff: %w", err)
		}
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
		if string(bytes.TrimSpace(line)) == judgeDirName {
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
	_, err = f.WriteString(judgeDirName + "\n")
	return err
}

// copyTree 递归拷贝 src 目录到 dst（跳过 manifest.json 与 sig），
// 文件权限 0o755 保证判分脚本可直接执行。
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
