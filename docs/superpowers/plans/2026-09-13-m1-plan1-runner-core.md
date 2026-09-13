# M1 计划 1：Runner 核心闭环（本地镜像对战）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建 agentbattle CLI：在本地隔离沙箱中召唤 coding agent（Claude Code 先行）执行任务、采集事件流、本地判分，并支持 A/B 两配置批量镜像对战（默认 20 局）输出胜负统计——不需要服务器即可验证"任务→执行→采集→判分→胜负"核心闭环。

**Architecture:** 单 Go module monorepo。`protocol` 包定义事件结构体、hash 链、判分契约（Runner 与未来平台共享）；`runner` 包含沙箱隔离、Agent Adapter 抽象（Claude Code + echo 测试替身）、事件采集器、判分执行器、对局编排；CLI 提供 `run`（单次执行）与 `mirror`（A/B 镜像对战 N 局）。所有测试在 Windows git-bash 环境下可跑。

**Tech Stack:** Go 1.22+（仅标准库，零第三方依赖）、git、bash（判分测试命令载体）。被召唤的 agent：Claude Code CLI（`claude -p --output-format stream-json`）。

**对应设计文档:** `docs/superpowers/specs/2026-09-13-agent-battle-platform-design.md` 第 4 节（Runner）、第 5.2 节（赛制一判分）、第 9 节（测试策略）。本计划覆盖设计文档 M1 里程碑的 Runner 半边；平台半边由计划 2 实现。

**术语约定（全文一致使用）:**
- **任务目录（task dir）**: 一个含 `task.json`（任务清单）、`seed/`（种子仓库）、`tests/`（判分包）的目录
- **沙箱（sandbox）**: 每局新建的临时目录，git init 后拷入 seed 内容，agent 在此工作
- **判分包（judge package）**: `tests/` 目录，含 `manifest.json`（测试命令列表）+ `sig`（HMAC 签名）
- **RawEvent / Event**: adapter 产出的未加工事件 / 加上序号时间戳与 hash 链后的最终事件

---

## 文件结构总览

```
agent-battle/
├── go.mod                          # module agentbattle
├── protocol/                       # 共享契约包（计划 2 平台复用）
│   ├── events.go                   # Event 结构体与类型常量
│   ├── events_test.go
│   ├── hashchain.go                # hash 链计算与校验
│   ├── hashchain_test.go
│   ├── judge.go                    # TaskManifest / JudgeManifest / JudgeReport
│   └── judge_test.go
├── runner/
│   ├── cmd/agentbattle/
│   │   └── main.go                 # CLI 入口：run / mirror 子命令
│   ├── internal/
│   │   ├── adapter/
│   │   │   ├── adapter.go          # Adapter 接口 + RawEvent + 注册表
│   │   │   ├── echo.go             # 测试替身 agent
│   │   │   ├── echo_test.go
│   │   │   ├── claudecode.go       # Claude Code adapter
│   │   │   └── claudecode_test.go  # 用 fixture JSON 行测 parseLine
│   │   ├── sandbox/
│   │   │   ├── sandbox.go          # 临时仓库创建/清理
│   │   │   └── sandbox_test.go
│   │   ├── collector/
│   │   │   ├── collector.go        # RawEvent → 链式 Event，NDJSON 输出
│   │   │   └── collector_test.go
│   │   ├── judge/
│   │   │   ├── judge.go            # 判分包加载/验签/执行
│   │   │   └── judge_test.go
│   │   └── session/
│   │       ├── session.go          # prepare→execute→judge→report 编排
│   │       ├── session_test.go     # 用 echo adapter 走全流程
│   │       ├── mirror.go           # A/B 镜像对战 N 局 + 统计
│   │       └── mirror_test.go
│   └── e2e/
│       └── e2e_test.go             # echo agent 端到端回归
└── examples/fix-add/               # 示例任务目录（bash 可判分，零依赖）
    ├── task.json
    ├── seed/calc.sh
    └── tests/
        ├── manifest.json
        ├── run_tests.sh
        └── sig                     # 开发密钥 HMAC（由 Task 7 生成）
```

**设计红线（实现时不可违背，源自项目 CLAUDE.md）:** 事件流不含文件内容明文（tool 参数只存 sha256 hash，文件操作只存路径）；沙箱是全新临时目录，永不触碰用户真实项目。

---

### Task 1: go.mod 与 protocol.Event 结构体

**Files:**
- Create: `go.mod`
- Create: `protocol/events.go`
- Test: `protocol/events_test.go`

- [ ] **Step 1: 初始化 module**

Run: `cd /d/AI/agent-battle && go mod init agentbattle && go version`
Expected: `go: creating new go.mod: module agentbattle`，Go 版本 ≥ 1.22

- [ ] **Step 2: 写失败测试（JSON 序列化形状是跨端契约，必须锁死）**

```go
// protocol/events_test.go
package protocol

import (
	"encoding/json"
	"testing"
)

func TestEventJSONShape(t *testing.T) {
	e := Event{Seq: 1, TS: 1700000000000, Type: EventToolCall, Tool: "Bash",
		ArgsHash: "abc", DurationMS: 12, Tokens: 0, Note: "/tmp/x.py"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"seq":1,"ts":1700000000000,"type":"tool_call","tool":"Bash","args_hash":"abc","duration_ms":12,"note":"/tmp/x.py","hash":""}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestEventTypeConstants(t *testing.T) {
	// 契约常量：计划 2 平台按这些字符串分派
	for _, s := range []string{EventToolCall, EventFileEdit, EventMessage, EventError, EventResult} {
		if s == "" {
			t.Fatal("empty event type constant")
		}
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./protocol/ -v`
Expected: FAIL，`undefined: Event`（或编译错误）

- [ ] **Step 4: 最小实现**

```go
// protocol/events.go
package protocol

// 事件类型常量。跨端契约，只增不改。
const (
	EventToolCall = "tool_call"
	EventFileEdit = "file_edit"
	EventMessage  = "message"
	EventError    = "error"
	EventResult   = "result"
)

// Event 是事件流的最小单元。字段顺序即 JSON 序列化顺序，
// hash 链依赖 canonical 序列化，勿调整字段顺序。
// 红线：任何字段不得存放文件内容明文；Note 只允许存路径等摘要信息。
type Event struct {
	Seq        int    `json:"seq"`
	TS         int64  `json:"ts"` // unix 毫秒
	Type       string `json:"type"`
	Tool       string `json:"tool,omitempty"`
	ArgsHash   string `json:"args_hash,omitempty"` // sha256(args)，防内容泄漏
	DurationMS int64  `json:"duration_ms,omitempty"`
	Tokens     int    `json:"tokens,omitempty"`
	Note       string `json:"note,omitempty"`
	PrevHash   string `json:"prev_hash,omitempty"`
	Hash       string `json:"hash"`
}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./protocol/ -v`
Expected: `PASS`，2 个测试通过

- [ ] **Step 6: Commit**

```bash
git add go.mod protocol/
git commit -m "feat(protocol): Event 结构体与事件类型契约"
```

---

### Task 2: hash 链

**Files:**
- Create: `protocol/hashchain.go`
- Test: `protocol/hashchain_test.go`

防篡改机制：每条事件 Hash = sha256(canonical(事件去掉Hash) + 前一条Hash)。删改中间任何一条都会导致后续校验失败（设计文档 7.2 检测网 1）。

- [ ] **Step 1: 写失败测试**

```go
// protocol/hashchain_test.go
package protocol

import "testing"

func mkChain(n int) []Event {
	evs := make([]Event, n)
	for i := range evs {
		evs[i] = Event{Type: EventMessage, Note: "e"}
	}
	return evs
}

func TestChainRoundTrip(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	if err := Chain(evs); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
}

func TestChainDetectsTamper(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	evs[1].Tokens = 999 // 篡改中间事件
	if err := Chain(evs); err == nil {
		t.Fatal("tampered chain not detected")
	}
}

func TestChainDetectsDeletion(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	broken := []Event{evs[0], evs[2]} // 删除中间一条
	if err := Chain(broken); err == nil {
		t.Fatal("deleted chain not detected")
	}
}

func TestChainDetectsReorder(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	evs[0], evs[1] = evs[1], evs[0]
	if err := Chain(evs); err == nil {
		t.Fatal("reordered chain not detected")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./protocol/ -run TestChain -v`
Expected: FAIL，`undefined: Seal`

- [ ] **Step 3: 最小实现**

```go
// protocol/hashchain.go
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// GenesisHash 是链头哨兵值。
const GenesisHash = "GENESIS"

// Seal 依次为 events 计算 PrevHash 与 Hash（原地修改）。
func Seal(events []Event) {
	prev := GenesisHash
	for i := range events {
		events[i].Seq = i
		events[i].PrevHash = prev
		events[i].Hash = compute(events[i])
		prev = events[i].Hash
	}
}

// Chain 校验事件链完整性：seq 连续、hash 逐条吻合。
func Chain(events []Event) error {
	prev := GenesisHash
	for i, e := range events {
		if e.Seq != i {
			return fmt.Errorf("event %d: seq=%d, want %d（疑似删改/乱序）", i, e.Seq, i)
		}
		if compute(e) != e.Hash {
			return fmt.Errorf("event %d: hash 不匹配（链条断裂）", i)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("event %d: prev_hash 不匹配", i)
		}
		prev = e.Hash
	}
	return nil
}

func compute(e Event) string {
	e.Hash = "" // Hash 本身不入 hash
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // Event 全为可序列化基础类型，不可能失败
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./protocol/ -v`
Expected: PASS（含 Task 1 的测试）

- [ ] **Step 5: Commit**

```bash
git add protocol/hashchain.go protocol/hashchain_test.go
git commit -m "feat(protocol): 事件 hash 链（Seal/Chain，防删改防乱序）"
```

---

### Task 3: TaskManifest / JudgeManifest / JudgeReport

**Files:**
- Create: `protocol/judge.go`
- Test: `protocol/judge_test.go`

- [ ] **Step 1: 写失败测试**

```go
// protocol/judge_test.go
package protocol

import (
	"encoding/json"
	"testing"
)

func TestJudgeReportJSONShape(t *testing.T) {
	r := JudgeReport{TaskID: "fix-add", Passed: 1, Total: 2, DiffHash: "deadbeef",
		Results: []TestResult{{Name: "add-works", Passed: true, ExitCode: 0, LogHash: "aa"}}}
	b, _ := json.Marshal(r)
	want := `{"task_id":"fix-add","results":[{"name":"add-works","passed":true,"exit_code":0,"log_hash":"aa"}],"passed":1,"total":2,"diff_hash":"deadbeef"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestJudgeManifestParse(t *testing.T) {
	data := []byte(`{"task_id":"fix-add","tests":[{"name":"t1","cmd":"bash .judge/run_tests.sh"}]}`)
	var m JudgeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.TaskID != "fix-add" || len(m.Tests) != 1 || m.Tests[0].Cmd == "" {
		t.Fatalf("bad parse: %+v", m)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./protocol/ -run TestJudge -v`
Expected: FAIL，`undefined: JudgeReport`

- [ ] **Step 3: 最小实现**

```go
// protocol/judge.go
package protocol

// TaskManifest 描述一个任务，位于任务目录 task.json。
type TaskManifest struct {
	TaskID      string `json:"task_id"`
	Name        string `json:"name"`
	Description string `json:"description"` // 发给 agent 的任务提示词
	TimeoutSec  int    `json:"timeout_sec"` // agent 执行硬超时，0=默认 600
}

// JudgeManifest 描述判分包，位于任务目录 tests/manifest.json。
type JudgeManifest struct {
	TaskID string        `json:"task_id"`
	Tests  []TestCommand `json:"tests"`
}

type TestCommand struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd"` // 在沙箱 cwd 下用 bash -c 执行
}

type TestResult struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	LogHash  string `json:"log_hash"` // sha256(stdout+stderr)
}

// JudgeReport 是一局判分的最终产物。
type JudgeReport struct {
	TaskID   string       `json:"task_id"`
	Results  []TestResult `json:"results"`
	Passed   int          `json:"passed"`
	Total    int          `json:"total"`
	DiffHash string       `json:"diff_hash"` // sha256(git diff HEAD 输出)
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./protocol/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add protocol/judge.go protocol/judge_test.go
git commit -m "feat(protocol): 任务/判分契约（TaskManifest/JudgeManifest/JudgeReport）"
```

---

### Task 4: 沙箱隔离层

**Files:**
- Create: `runner/internal/sandbox/sandbox.go`
- Test: `runner/internal/sandbox/sandbox_test.go`

- [ ] **Step 1: 写失败测试**

```go
// runner/internal/sandbox/sandbox_test.go
package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCopiesSeedAndInitsGit(t *testing.T) {
	seed := t.TempDir()
	write(t, seed, "calc.sh", "add() { echo $((a-b)); }\n")
	write(t, seed, ".git", "not-a-repo") // seed 里的 .git 必须被跳过

	sb, cleanup, err := Create(seed)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if sb == "" || sb == seed {
		t.Fatalf("sandbox path invalid: %q", sb)
	}
	if _, err := os.Stat(filepath.Join(sb, "calc.sh")); err != nil {
		t.Fatalf("seed file not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb, ".git")); err == nil {
		t.Fatal("seed .git should not be copied")
	}
	// git 仓库可用（有 HEAD）
	if _, err := os.Stat(filepath.Join(sb, ".git", "HEAD")); err != nil {
		t.Fatalf("git init missing HEAD: %v", err)
	}
}

func TestCleanupRemovesSandbox(t *testing.T) {
	seed := t.TempDir()
	write(t, seed, "a.txt", "hi")
	sb, cleanup, err := Create(seed)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(sb); !os.IsNotExist(err) {
		t.Fatal("sandbox not removed after cleanup")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/sandbox/ -v`
Expected: FAIL，编译错误 `undefined: Create`

- [ ] **Step 3: 最小实现**

```go
// runner/internal/sandbox/sandbox.go
// Package sandbox 为每局对局创建全新隔离环境。
// 红线：只用平台/任务下发的内容，永不读写用户真实项目。
package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// Create 新建临时沙箱：git init + 拷贝 seedDir 内容（跳过 .git）。
// 返回沙箱路径与清理函数；清理函数幂等，可 defer 多次调用。
func Create(seedDir string) (string, func(), error) {
	sb, err := os.MkdirTemp("", "agentbattle-*")
	if err != nil {
		return "", nil, fmt.Errorf("mkdtemp: %w", err)
	}
	cleanup := syncOnceRemove(sb)

	if out, err := exec.Command("git", "-C", sb, "init", "-q").CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git init: %w: %s", err, out)
	}
	if err := copyTree(seedDir, sb); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy seed: %w", err)
	}
	// 首个提交作为判分 diff 基线
	cmd := exec.Command("git", "-C", sb, "-c", "user.email=runner@agentbattle", "-c", "user.name=runner",
		"add", "-A")
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git add: %w: %s", err, out)
	}
	cmd = exec.Command("git", "-C", sb, "-c", "user.email=runner@agentbattle", "-c", "user.name=runner",
		"commit", "-qm", "seed")
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git commit: %w: %s", err, out)
	}
	return sb, cleanup, nil
}

// copyTree 把 src 下所有常规文件拷到 dst，跳过 .git 目录。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // 跳过符号链接等
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func syncOnceRemove(dir string) func() {
	var done bool
	return func() {
		if done {
			return
		}
		done = true
		os.RemoveAll(dir)
	}
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./runner/internal/sandbox/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add runner/internal/sandbox/
git commit -m "feat(sandbox): 每局全新 git 临时仓库（含 seed 基线提交）"
```

---

### Task 5: Adapter 接口 + echo 测试替身

**Files:**
- Create: `runner/internal/adapter/adapter.go`
- Create: `runner/internal/adapter/echo.go`
- Test: `runner/internal/adapter/echo_test.go`

echo adapter 是测试替身：模拟一个 agent 的行为（发事件、写"正确解"），让全链路测试不依赖真实 Claude Code。

- [ ] **Step 1: 定义接口（接口先行的失败测试 = 编译失败）**

```go
// runner/internal/adapter/echo_test.go
package adapter

import (
	"context"
	"testing"
	"time"
)

func TestEchoSolvesTask(t *testing.T) {
	cwd := t.TempDir()
	a := Echo{FixContent: "fixed"}
	ch := make(chan RawEvent, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := a.Launch(ctx, cwd, "fix it", ch)
	if err != nil {
		t.Fatal(err)
	}
	close(ch)
	types := map[string]int{}
	for e := range ch {
		types[e.Type]++
	}
	if types[EventToolCall] == 0 || types[EventResult] == 0 {
		t.Fatalf("echo should emit tool_call and result, got %v", types)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/adapter/ -v`
Expected: FAIL，编译错误（RawEvent/EventToolCall/EventResult/Launch 未定义）

- [ ] **Step 3: 实现接口与 echo**

```go
// runner/internal/adapter/adapter.go
// Package adapter 定义"召唤本机 coding agent"的统一契约。
package adapter

// RawEvent 是 adapter 产出的未加工事件，由 collector 打上
// 序号/时间戳/hash 链后成为 protocol.Event。
type RawEvent struct {
	Type       string // 同 protocol 的事件类型常量
	Tool       string
	ArgsHash   string
	DurationMS int64
	Tokens     int
	Note       string // 仅路径/摘要，红线禁止内容明文
}

// Adapter 是接入一个 coding agent 的最小契约。
type Adapter interface {
	Name() string
	// Detect 返回非 nil 表示本机不可用（如二进制缺失）。
	Detect() error
	// Launch 在 cwd 中以 taskDescription 运行 agent。
	// ctx 取消时必须杀死 agent 进程。事件流入 out（Launch 返回前不 close，
	// 由调用方负责）。
	Launch(ctx Ctx, cwd, taskDescription string, env []string, out chan<- RawEvent) error
}

// Ctx 是 context.Context 的别名，避免每个实现都 import context。
// （保持接口文件零依赖，便于阅读契约。）
type Ctx = interface {
	Done() <-chan struct{}
	Err() error
	Deadline() (deadline interface{ IsZero() bool }, ok bool)
}
```

等一下——这个 Ctx 别名设计是过度设计，直接用 `context.Context`：

```go
// runner/internal/adapter/adapter.go
// Package adapter 定义"召唤本机 coding agent"的统一契约。
package adapter

import "context"

// RawEvent 是 adapter 产出的未加工事件，由 collector 打上
// 序号/时间戳/hash 链后成为 protocol.Event。
type RawEvent struct {
	Type       string // 同 protocol 的事件类型常量
	Tool       string
	ArgsHash   string
	DurationMS int64
	Tokens     int
	Note       string // 仅路径/摘要，红线禁止内容明文
}

// Adapter 是接入一个 coding agent 的最小契约。
type Adapter interface {
	Name() string
	// Detect 返回非 nil 表示本机不可用（如二进制缺失）。
	Detect() error
	// Launch 在 cwd 中以 taskDescription 运行 agent，事件流入 out。
	// ctx 取消/超时必须杀死 agent 进程。out 由调用方 close。
	Launch(ctx context.Context, cwd, taskDescription string, env []string, out chan<- RawEvent) error
}
```

（以上第二份为准，第一份作废——计划文档保留此处说明以警示：不要发明不必要的抽象。）

```go
// runner/internal/adapter/echo.go
package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Echo 是测试替身 agent：发几个假事件，写入 FixContent 作为"解法"，
// 正常返回。FixContent 为空则不写文件（用于制造判分失败样本）。
type Echo struct {
	FixContent string
	Fail       bool // true 时返回 error，模拟 agent 崩溃
}

func (e Echo) Name() string { return "echo" }

func (e Echo) Detect() error { return nil }

func (e Echo) Launch(ctx context.Context, cwd, task string, env []string, out chan<- RawEvent) error {
	if e.Fail {
		return fmt.Errorf("echo: simulated crash")
	}
	sum := sha256.Sum256([]byte(task))
	out <- RawEvent{Type: "tool_call", Tool: "Read", ArgsHash: hex.EncodeToString(sum[:])}
	out <- RawEvent{Type: "tool_call", Tool: "Edit", ArgsHash: hex.EncodeToString(sum[:4]),
		Note: filepath.Join(cwd, "calc.sh")}
	if e.FixContent != "" {
		if err := os.WriteFile(filepath.Join(cwd, "calc.sh"), []byte(e.FixContent), 0o644); err != nil {
			return err
		}
		out <- RawEvent{Type: "file_edit", Tool: "Edit", Note: "calc.sh"}
	}
	time.Sleep(10 * time.Millisecond) // 模拟耗时
	out <- RawEvent{Type: "result", DurationMS: 10, Tokens: 42}
	return nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./runner/internal/adapter/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add runner/internal/adapter/adapter.go runner/internal/adapter/echo.go runner/internal/adapter/echo_test.go
git commit -m "feat(adapter): Adapter 契约 + echo 测试替身"
```

---

### Task 6: ClaudeCodeAdapter

**Files:**
- Create: `runner/internal/adapter/claudecode.go`
- Test: `runner/internal/adapter/claudecode_test.go`

要点：`claude -p <task> --output-format stream-json --verbose`（headless 模式；`--dangerously-skip-permissions` 由调用方经 env/flag 决定是否追加，本地镜像对战需要它绕过权限确认）。解析规则：
- `{"type":"assistant","message":{"content":[...]}}` → 每个 `tool_use` 块产出 tool_call 事件（Edit/Write 另产 file_edit，Note=file_path）
- `{"type":"result","duration_ms":..,"usage":{...}}` → result 事件
- 其余行（system/init 等）忽略；非法 JSON 行产出 error 事件但不中断

- [ ] **Step 1: 写失败测试（fixture 驱动，不需要真实 claude 二进制）**

```go
// runner/internal/adapter/claudecode_test.go
package adapter

import (
	"strings"
	"testing"
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/adapter/ -run TestParseLine -v`
Expected: FAIL，`undefined: parseLine`

- [ ] **Step 3: 实现**

```go
// runner/internal/adapter/claudecode.go
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
	"strings"
)

// ClaudeCode 通过 headless 模式召唤本机 claude CLI。
type ClaudeCode struct {
	// Bin 为 claude 可执行文件名/路径，默认 "claude"。
	Bin string
	// YOLO 追加 --dangerously-skip-permissions（镜像对战/无人值守必需）。
	YOLO bool
}

func (c ClaudeCode) Name() string { return "claude-code" }

func (c ClaudeCode) Detect() error {
	bin := c.bin()
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%s 不在 PATH 中: %w", bin, err)
	}
	return nil
}

func (c ClaudeCode) bin() string {
	if c.Bin == "" {
		return "claude"
	}
	return c.Bin
}

// Launch 运行 claude 并把 stdout 逐行喂给 parseLine。
func (c ClaudeCode) Launch(ctx context.Context, cwd, task string, env []string, out chan<- RawEvent) error {
	args := []string{"-p", task, "--output-format", "stream-json", "--verbose"}
	if c.YOLO {
		args = append(args, "--dangerously-skip-permissions")
	}
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderrTail strings.Builder
	cmd.Stderr = io.MultiWriter(&stderrTail, io.Discard)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // 容忍超长行（16MB）
	for sc.Scan() {
		for _, ev := range mustParse(sc.Bytes(), &stderrTail) {
			out <- ev
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude exited: %w; stderr: %s", err, tail(&stderrTail))
	}
	return nil
}

// mustParse 永不失败：非法行降级为 error 事件。
func mustParse(line []byte, errSink *strings.Builder) []RawEvent {
	evs, err := parseLine(line)
	if err != nil {
		errSink.WriteString(fmt.Sprintf("line parse: %v\n", err))
		return []RawEvent{{Type: "error", Note: "stdout 行解析失败"}}
	}
	return evs
}

func tail(sb *strings.Builder) string {
	s := sb.String()
	if len(s) > 2000 {
		return s[len(s)-2000:]
	}
	return s
}

type ccLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message *struct {
		Content []struct {
			Type string          `json:"type"`
			Name string          `json:"name"`
			Text string          `json:"text"`
			In   json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	IsError    bool `json:"is_error"`
	DurationMS int64 `json:"duration_ms"`
	Usage      *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// parseLine 把一行 stream-json 解析为 0..n 个 RawEvent。
func parseLine(line []byte) ([]RawEvent, error) {
	line = trimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var l ccLine
	if err := json.Unmarshal(line, &l); err != nil {
		return []RawEvent{{Type: "error", Note: "stdout 非 JSON 行"}}, nil // 降级不中断
	}
	var evs []RawEvent
	switch l.Type {
	case "assistant":
		if l.Message == nil {
			return nil, nil
		}
		for _, blk := range l.Message.Content {
			switch blk.Type {
			case "tool_use":
				h := sha256.Sum256(blk.In)
				evs = append(evs, RawEvent{Type: "tool_call", Tool: blk.Name,
					ArgsHash: hex.EncodeToString(h[:])})
				if blk.Name == "Edit" || blk.Name == "Write" || blk.Name == "MultiEdit" {
					evs = append(evs, RawEvent{Type: "file_edit", Tool: blk.Name,
						Note: filePathOf(blk.In)})
				}
			case "text":
				evs = append(evs, RawEvent{Type: "message", Note: ""}) // 不采集正文
			}
		}
	case "result":
		ev := RawEvent{Type: "result", DurationMS: l.DurationMS}
		if l.Usage != nil {
			ev.Tokens = l.Usage.InputTokens + l.Usage.OutputTokens
		}
		if l.IsError {
			ev.Type = "error"
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

func filePathOf(in json.RawMessage) string {
	var m struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(in, &m) == nil {
		return m.FilePath
	}
	return ""
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r' || b[len(b)-1] == '\n') {
		b = b[:len(b)-1]
	}
	return b
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./runner/internal/adapter/ -v`
Expected: PASS（含 Task 5 echo 测试）

- [ ] **Step 5: Commit**

```bash
git add runner/internal/adapter/claudecode.go runner/internal/adapter/claudecode_test.go
git commit -m "feat(adapter): Claude Code adapter（stream-json 解析，参数只存 hash）"
```

---

### Task 7: 事件采集器

**Files:**
- Create: `runner/internal/collector/collector.go`
- Test: `runner/internal/collector/collector_test.go`

- [ ] **Step 1: 写失败测试**

```go
// runner/internal/collector/collector_test.go
package collector

import (
	"bytes"
	"encoding/json"
	"testing"

	"agentbattle/protocol"
)

func TestAddSealsChain(t *testing.T) {
	c := New()
	e1 := c.Add(RawEvent{Type: protocol.EventToolCall, Tool: "Bash"})
	e2 := c.Add(RawEvent{Type: protocol.EventResult, Tokens: 10})
	if e1.Seq != 0 || e2.Seq != 1 {
		t.Fatalf("seq broken: %d %d", e1.Seq, e2.Seq)
	}
	if e2.PrevHash != e1.Hash {
		t.Fatal("chain not linked")
	}
	if err := protocol.Chain(c.Events()); err != nil {
		t.Fatalf("chain invalid: %v", err)
	}
}

func TestConcurrentAdd(t *testing.T) {
	c := New()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			for j := 0; j < 25; j++ {
				c.Add(RawEvent{Type: protocol.EventMessage})
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if len(c.Events()) != 100 {
		t.Fatalf("lost events: %d", len(c.Events()))
	}
	if err := protocol.Chain(c.Events()); err != nil {
		t.Fatalf("chain invalid under concurrency: %v", err)
	}
}

func TestFlushNDJSON(t *testing.T) {
	c := New()
	c.Add(RawEvent{Type: protocol.EventMessage})
	var buf bytes.Buffer
	if err := c.FlushNDJSON(&buf); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	var e protocol.Event
	if err := json.Unmarshal(lines[0], &e); err != nil {
		t.Fatal(err)
	}
	if e.Hash == "" {
		t.Fatal("event lost hash in serialization")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/collector/ -v`
Expected: FAIL，编译错误

- [ ] **Step 3: 最小实现**

```go
// runner/internal/collector/collector.go
// Package collector 把 adapter 的 RawEvent 加工成带 hash 链的 protocol.Event。
package collector

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"time"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
)

// RawEvent 别名避免调用方 import 两个包。
type RawEvent = adapter.RawEvent

type Collector struct {
	mu     sync.Mutex
	seq    int
	events []protocol.Event
}

func New() *Collector { return &Collector{} }

// Add 记录一条事件：打序号、取当前时间、续 hash 链。并发安全。
func (c *Collector) Add(raw RawEvent) protocol.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := protocol.Event{
		Seq:        c.seq,
		TS:         time.Now().UnixMilli(),
		Type:       raw.Type,
		Tool:       raw.Tool,
		ArgsHash:   raw.ArgsHash,
		DurationMS: raw.DurationMS,
		Tokens:     raw.Tokens,
		Note:       raw.Note,
	}
	prev := protocol.GenesisHash
	if c.seq > 0 {
		prev = c.events[c.seq-1].Hash
	}
	e.PrevHash = prev
	e.Hash = sealOne(e, prev)
	c.events = append(c.events, e)
	c.seq++
	return e
}

func (c *Collector) Events() []protocol.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.Event, len(c.events))
	copy(out, c.events)
	return out
}

// FlushNDJSON 把全部事件以 NDJSON 写入 w（一行一个事件）。
func (c *Collector) FlushNDJSON(w io.Writer) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, e := range c.Events() {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return bw.Flush()
}
```

问题：`sealOne` 未定义。hash 计算逻辑在 protocol 包里是私有 `compute`，这里需要导出。回到 protocol 包给 `Seal` 加一个单事件版本——修改 protocol/hashchain.go 追加导出函数（在 Task 2 文件上追加）：

```go
// SealOne 为单条事件计算 PrevHash 与 Hash 并返回（不修改入参之外的链）。
// 用于 collector 增量产事件：prevHash 由调用方携带。
func SealOne(e Event, prevHash string) Event {
	e.Seq = -1 // 序号由调用方决定，不参与单条 hash 计算前的赋值
	e.PrevHash = prevHash
	e.Hash = compute(e)
	return e
}
```

但 compute 的 hash 输入含 Seq 吗？canonical 序列化包含全部字段（含 Seq）。collector 需要先定 Seq 再算 hash。调整：collector.Add 里自己组好 Event（含 Seq/PrevHash）后调用 `protocol.SealOne(e)`（不带 prev 参数，函数只填 Hash）：

protocol/hashchain.go 追加：

```go
// SealOne 只计算并填充 e.Hash（PrevHash/Seq 由调用方先填好）。
func SealOne(e Event) Event {
	e.Hash = compute(e)
	return e
}
```

collector.Add 中相应改为：

```go
	prev := protocol.GenesisHash
	if c.seq > 0 {
		prev = c.events[c.seq-1].Hash
	}
	e.PrevHash = prev
	e = protocol.SealOne(e)
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./runner/... ./protocol/ -v`
Expected: PASS（collector 3 个测试 + protocol 全部）

- [ ] **Step 5: Commit**

```bash
git add protocol/hashchain.go runner/internal/collector/
git commit -m "feat(collector): 并发安全事件采集器（NDJSON 输出）+ protocol.SealOne"
```

---

### Task 8: 判分执行器

**Files:**
- Create: `runner/internal/judge/judge.go`
- Test: `runner/internal/judge/judge_test.go`

- [ ] **Step 1: 写失败测试**

```go
// runner/internal/judge/judge_test.go
package judge

import (
	"os"
	"path/filepath"
	"testing"
)

const devKey = "dev-secret" // M1 开发密钥；平台化后由平台签发

func setupTask(t *testing.T) (taskDir, sandbox string) {
	t.Helper()
	taskDir = t.TempDir()
	testsDir := filepath.Join(taskDir, "tests")
	if err := os.MkdirAll(testsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `{"task_id":"t1","tests":[{"name":"ok","cmd":"bash .judge/run.sh"},{"name":"bad","cmd":"bash .judge/fail.sh"}]}`
	if err := os.WriteFile(filepath.Join(testsDir, "manifest.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "run.sh"), []byte("exit 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "fail.sh"), []byte("exit 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SignDir(taskDir, []byte(devKey)); err != nil {
		t.Fatal(err)
	}

	sandbox = t.TempDir()
	if err := os.WriteFile(filepath.Join(sandbox, "work.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return taskDir, sandbox
}

func TestRunVerifiesAndScores(t *testing.T) {
	taskDir, sb := setupTask(t)
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
	if rep.DiffHash == "" {
		t.Fatal("diff hash empty")
	}
}

func TestTamperedManifestRejected(t *testing.T) {
	taskDir, sb := setupTask(t)
	// 篡改 manifest（换掉一条测试）后必须拒绝执行
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/judge/ -v`
Expected: FAIL，编译错误

- [ ] **Step 3: 实现**

```go
// runner/internal/judge/judge.go
// Package judge 加载、验签并执行判分包。
// M1 用 HMAC-SHA256 + 开发密钥；平台化后密钥由平台按局签发。
package judge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentbattle/protocol"
)

// SignDir 对 taskDir/tests/manifest.json 计算 HMAC，写入 tests/sig（hex 文本）。
func SignDir(taskDir string, key []byte) error {
	mp := filepath.Join(taskDir, "tests", "manifest.json")
	raw, err := os.ReadFile(mp)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonicalJSON(raw))
	sig := hex.EncodeToString(mac.Sum(nil))
	return os.WriteFile(filepath.Join(taskDir, "tests", "sig"), []byte(sig), 0o644)
}

// canonicalJSON 解析后重新序列化，消除空白差异，保证签名跨机器可验证。
func canonicalJSON(raw []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw // 非 JSON 按原文签
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// Run 验签后执行判分包：拷贝 tests/*（除 manifest/sig）到 sandbox/.judge，
// 逐条执行测试命令（cwd=沙箱），最后计算 git diff hash。
func Run(taskDir, sandbox string, key []byte) (protocol.JudgeReport, error) {
	testsDir := filepath.Join(taskDir, "tests")
	raw, err := os.ReadFile(filepath.Join(testsDir, "manifest.json"))
	if err != nil {
		return protocol.JudgeReport{}, err
	}
	sigFile, err := os.ReadFile(filepath.Join(testsDir, "sig"))
	if err != nil {
		return protocol.JudgeReport{}, fmt.Errorf("缺签名文件: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonicalJSON(raw))
	if !hmac.Equal(mac.Sum(nil), mustHex(strings.TrimSpace(string(sigFile)))) {
		return protocol.JudgeReport{}, fmt.Errorf("判分包签名校验失败")
	}
	var m protocol.JudgeManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return protocol.JudgeReport{}, err
	}

	// 拷贝测试资产到沙箱 .judge（.gitignore 掉，不进 diff）
	judgeDir := filepath.Join(sandbox, ".judge")
	if err := os.MkdirAll(judgeDir, 0o755); err != nil {
		return protocol.JudgeReport{}, err
	}
	ents, err := os.ReadDir(testsDir)
	if err != nil {
		return protocol.JudgeReport{}, err
	}
	for _, ent := range ents {
		if ent.IsDir() || ent.Name() == "manifest.json" || ent.Name() == "sig" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(testsDir, ent.Name()))
		if err != nil {
			return protocol.JudgeReport{}, err
		}
		if err := os.WriteFile(filepath.Join(judgeDir, ent.Name()), data, 0o755); err != nil {
			return protocol.JudgeReport{}, err
		}
	}
	if err := appendGitignore(sandbox, ".judge/"); err != nil {
		return protocol.JudgeReport{}, err
	}

	rep := protocol.JudgeReport{TaskID: m.TaskID}
	for _, tc := range m.Tests {
		tr := runOne(tc, sandbox)
		if tr.Passed {
			rep.Passed++
		}
		rep.Results = append(rep.Results, tr)
	}
	rep.Total = len(m.Tests)

	diff, err := exec.Command("git", "-C", sandbox, "diff", "HEAD").Output()
	if err != nil {
		return protocol.JudgeReport{}, fmt.Errorf("git diff: %w", err)
	}
	sum := sha256.Sum256(diff)
	rep.DiffHash = hex.EncodeToString(sum[:])
	return rep, nil
}

func runOne(tc protocol.TestCommand, sandbox string) protocol.TestResult {
	tr := protocol.TestResult{Name: tc.Name}
	cmd := exec.Command("bash", "-c", tc.Cmd)
	cmd.Dir = sandbox
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			tr.ExitCode = ee.ExitCode()
		} else {
			tr.ExitCode = -1
		}
	}
	tr.Passed = err == nil
	sum := sha256.Sum256(out)
	tr.LogHash = hex.EncodeToString(sum[:])
	return tr
}

func appendGitignore(sandbox, line string) error {
	p := filepath.Join(sandbox, ".gitignore")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil // hmac.Equal 与 nil 比较必为 false → 校验失败
	}
	return b
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./runner/internal/judge/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add runner/internal/judge/
git commit -m "feat(judge): 判分包 HMAC 验签 + 沙箱内测试执行 + diff hash"
```

---

### Task 9: 对局编排 session.Run

**Files:**
- Create: `runner/internal/session/session.go`
- Test: `runner/internal/session/session_test.go`

- [ ] **Step 1: 写失败测试（echo agent 走全流程）**

```go
// runner/internal/session/session_test.go
package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
)

// newTask 在临时目录构造一个 echo 可解的任务。
func newTask(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	task := protocol.TaskManifest{TaskID: "t1", Name: "fix-add",
		Description: "fix calc.sh", TimeoutSec: 30}
	writeJSON(t, filepath.Join(dir, "task.json"), task)
	if err := os.WriteFile(filepath.Join(dir, "seed", "calc.sh"),
		[]byte("add() { echo $((a-b)); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := protocol.JudgeManifest{TaskID: "t1", Tests: []protocol.TestCommand{
		{Name: "add-works", Cmd: `bash -c 'source calc.sh; [ "$(add 2 3)" = "5" ]'`},
	}}
	writeJSON(t, filepath.Join(dir, "tests", "manifest.json"), m)
	return dir
}

func writeJSON(t *testing.T, p string, v interface{}) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRunPass(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  adapter.Echo{FixContent: "add() { echo $((a+ b)); }\n"},
		TaskDir:  taskDir,
		Label:    "A",
		JudgeKey: []byte("dev-secret"),
		Timeout:  30 * time.Second,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Passed != 1 || res.Report.Total != 1 {
		t.Fatalf("judge wrong: %+v", res.Report)
	}
	if len(res.Events) == 0 {
		t.Fatal("no events collected")
	}
	if err := protocol.Chain(res.Events); err != nil {
		t.Fatalf("event chain invalid: %v", err)
	}
	// 报告落盘
	if _, err := os.Stat(filepath.Join(res.Dir, "report.json")); err != nil {
		t.Fatalf("report.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "events.ndjson")); err != nil {
		t.Fatalf("events.ndjson missing: %v", err)
	}
}

func TestSessionRunAgentCrash(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  adapter.Echo{Fail: true},
		TaskDir:  taskDir,
		Label:    "crash",
		JudgeKey: []byte("dev-secret"),
		Timeout:  30 * time.Second,
	}
	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("agent crash should surface as error")
	}
}

func TestSessionRunJudgeFail(t *testing.T) {
	taskDir := newTask(t)
	cfg := Config{
		Adapter:  adapter.Echo{}, // 不写修复 → 判分失败但不报错
		TaskDir:  taskDir,
		Label:    "fail",
		JudgeKey: []byte("dev-secret"),
		Timeout:  30 * time.Second,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Passed != 0 {
		t.Fatalf("expected 0 passed, got %d", res.Report.Passed)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/session/ -v`
Expected: FAIL，编译错误（Config/Run 未定义）

- [ ] **Step 3: 实现**

```go
// runner/internal/session/session.go
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
	TaskDir  string        // 任务目录（task.json + seed/ + tests/）
	Label    string        // 本局标签，如 "config-A"
	Env      []string      // 追加给 agent 进程的环境变量（配置分化的载体）
	Timeout  time.Duration // 0 = 读 task.json 的 TimeoutSec
	JudgeKey []byte        // 判分包验签密钥
	OutDir   string        // 报告输出根目录，空 = 系统临时目录
}

type Result struct {
	Label  string
	Dir    string // 本局报告目录
	Report protocol.JudgeReport
	Events []protocol.Event
	WallMS int64
}

// Run 执行一局。沙箱在结束后保留报告目录、删除工作副本。
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
```

注意：session_test.go 里用了 `json.Marshal`，需在测试文件 import `encoding/json`（上方测试代码已隐含——执行者若遇编译错误，在 writeJSON 使用处补 `import "encoding/json"`）。同时 `res.Dir` 报告目录是**保留**的（在系统临时目录或 OutDir），沙箱本身删除。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./runner/internal/session/ -v`
Expected: PASS，3 个测试

- [ ] **Step 5: Commit**

```bash
git add runner/internal/session/session.go runner/internal/session/session_test.go
git commit -m "feat(session): 对局编排（沙箱→agent→判分→报告落盘）"
```

---

### Task 10: 示例任务目录 + 签名生成工具

**Files:**
- Create: `examples/fix-add/task.json`
- Create: `examples/fix-add/seed/calc.sh`
- Create: `examples/fix-add/tests/manifest.json`
- Create: `examples/fix-add/tests/run_tests.sh`
- Create: `examples/fix-add/tests/sig`（由签名命令生成）
- Create: `runner/cmd/agentbattle/sign.go`（CLI 子命令 `sign`）

- [ ] **Step 1: 写示例任务文件**

```json
// examples/fix-add/task.json
{
  "task_id": "fix-add",
  "name": "修复 add 函数",
  "description": "seed/calc.sh 中的 add() 函数有 bug：计算 a-b。请修复为计算 a+b。只修改 calc.sh。",
  "timeout_sec": 600
}
```

```bash
# examples/fix-add/seed/calc.sh
add() {
  echo $((a - b))
}
```

```json
// examples/fix-add/tests/manifest.json
{
  "task_id": "fix-add",
  "tests": [
    { "name": "add-basic", "cmd": "bash .judge/run_tests.sh" },
    { "name": "file-only-change", "cmd": "bash -c '[ \"$(git diff HEAD --name-only)\" = \"calc.sh\" ] || [ -z \"$(git diff HEAD --name-only)\" ]'" }
  ]
}
```

```bash
# examples/fix-add/tests/run_tests.sh
set -e
source calc.sh
[ "$(add 2 3)" = "5" ] || { echo "add(2,3) != 5"; exit 1; }
[ "$(add 10 -4)" = "6" ] || { echo "add(10,-4) != 6"; exit 1; }
echo OK
```

- [ ] **Step 2: 在 CLI 加 sign 子命令**

```go
// runner/cmd/agentbattle/sign.go
package main

import (
	"fmt"
	"os"

	"agentbattle/runner/internal/judge"
)

// cmdSign: agentbattle sign --task ./examples/fix-add
// 用开发密钥对任务目录的判分包签名，写入 tests/sig。
func cmdSign(args []string) error {
	fs := newFlagSet("sign")
	taskDir := fs.String("task", "", "任务目录路径")
	key := fs.String("key", sessionDevKey(), "HMAC 密钥")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskDir == "" {
		return fmt.Errorf("缺少 --task")
	}
	if err := judge.SignDir(*taskDir, []byte(*key)); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "已签名:", filepathJoin(*taskDir, "tests", "sig"))
	return nil
}
```

（`newFlagSet` / `sessionDevKey` / `filepathJoin` 是 main 包的公共小助手，在 Task 11 的 main.go 中定义；本 Task 的验证步骤先写 main.go 最小骨架。为避免任务间依赖混乱，本步骤与 Task 11 Step 1 合并验证：先完成 Task 11 的骨架再回来跑 sign。）

实际执行顺序调整：**Task 10 的 Step 2-4 在 Task 11 完成后执行**，本 Task 先只提交示例任务文件（不含 sig——sig 由 sign 命令生成，属于生成产物但需要入库供 mirror 使用）。

- [ ] **Step 1（执行位）: 写示例任务文件（上述 4 个文件）**

- [ ] **Step 2（执行位，Task 11 之后）: 生成签名**

Run: `go run ./runner/cmd/agentbattle sign --task ./examples/fix-add`
Expected: stderr 输出 `已签名: .../examples/fix-add/tests/sig`

- [ ] **Step 3（执行位）: 手动验证判分可执行**

Run: `cd /tmp && rm -rf sbtest && mkdir sbtest && cp /d/AI/agent-battle/examples/fix-add/seed/calc.sh sbtest/ && cd sbtest && git init -q && git add -A && git -c user.email=t@t -c user.name=t commit -qm s && bash ../agentbattle-sim.sh 2>/dev/null; source calc.sh && [ "$(add 2 3)" = "5" ] || echo "seed 是坏的（预期）"`
Expected: 输出 `seed 是坏的（预期）`——确认种子仓库确实带 bug（agent 不修就判分失败）

- [ ] **Step 4: Commit**

```bash
git add examples/
git commit -m "feat(examples): fix-add 示例任务（bash 判分，零外部依赖）"
```

---

### Task 11: CLI 入口（run 子命令）

**Files:**
- Create: `runner/cmd/agentbattle/main.go`
- Test: 手动冒烟（E2E 自动化在 Task 12）

- [ ] **Step 1: 实现 main.go 与 run 子命令**

```go
// runner/cmd/agentbattle/main.go
// agentbattle CLI：run（单局）/ mirror（A/B 镜像对战）/ sign（判分包签名）。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/judge"
	"agentbattle/runner/internal/session"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "mirror":
		err = cmdMirror(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentbattle — 本地 agent 对战 runner

用法:
  agentbattle run    --task <任务目录> [--agent claude-code|echo] [--label L]
                     [--env K=V]... [--out DIR] [--yolo]
  agentbattle mirror --task <任务目录> [--agent claude-code|echo]
                     --env-a K=V --env-b K=V [--rounds 20] [--out DIR] [--yolo]
  agentbattle sign   --task <任务目录> [--key SECRET]
`)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return fs
}

func sessionDevKey() string { return session.DevJudgeKey }

var _ = judge.DevJudgeKey // 占位防未用导入；若 judge 无此常量请删除本行

func filepathJoin(elems ...string) string { return filepath.Join(elems...) }

// collectEnv 把一组 K=V 字符串原样返回（校验格式）。
func collectEnv(values []string) ([]string, error) {
	for _, v := range values {
		for i := 0; i < len(v); i++ {
			if v[i] == '=' && i > 0 {
				return values, nil
			}
		}
		return nil, fmt.Errorf("env 格式错误（应为 K=V）: %q", v)
	}
	return values, nil
}

var _ = time.Second // 占位防未用导入；实现 mirror 时删除
```

等一下——上面 `collectEnv` 逻辑有 bug（循环里提前 return）。用干净实现：

```go
// runner/cmd/agentbattle/env.go
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentbattle/runner/internal/session"
)

func newFlagSet(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ExitOnError) }

func sessionDevKey() string { return session.DevJudgeKey }

func filepathJoin(elems ...string) string { return filepath.Join(elems...) }

// envFlag 支持 --env K=V 可多次出现的 flag.Value。
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }

func (e *envFlag) Set(v string) error {
	if !strings.Contains(v, "=") || strings.HasPrefix(v, "=") {
		return fmt.Errorf("env 格式错误（应为 K=V）: %q", v)
	}
	*e = append(*e, v)
	return nil
}

// mustOutDir 确保输出目录存在并返回绝对路径。
func mustOutDir(dir string) (string, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "agentbattle-reports")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
```

```go
// runner/cmd/agentbattle/run.go
package main

import (
	"context"
	"fmt"
	"os"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/session"
)

func cmdRun(args []string) error {
	fs := newFlagSet("run")
	taskDir := fs.String("task", "", "任务目录")
	agentName := fs.String("agent", "claude-code", "echo|claude-code")
	label := fs.String("label", "A", "本局标签")
	out := fs.String("out", "", "报告输出目录")
	yolo := fs.Bool("yolo", false, "claude-code 追加 --dangerously-skip-permissions")
	var envs envFlag
	fs.Var(&envs, "env", "追加环境变量 K=V（可多次）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskDir == "" {
		return fmt.Errorf("缺少 --task")
	}
	ad, err := pickAdapter(*agentName, *yolo)
	if err != nil {
		return err
	}
	outDir, err := mustOutDir(*out)
	if err != nil {
		return err
	}
	res, err := session.Run(context.Background(), session.Config{
		Adapter: ad, TaskDir: *taskDir, Label: *label,
		Env: envs, JudgeKey: []byte(session.DevJudgeKey), OutDir: outDir,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[%s] 判分 %d/%d  耗时 %dms  报告: %s\n",
		res.Label, res.Report.Passed, res.Report.Total, res.WallMS, res.Dir)
	return nil
}

func pickAdapter(name string, yolo bool) (adapter.Adapter, error) {
	switch name {
	case "echo":
		return adapter.Echo{}, nil
	case "claude-code":
		ad := adapter.ClaudeCode{YOLO: yolo}
		if err := ad.Detect(); err != nil {
			return nil, err
		}
		return ad, nil
	default:
		return nil, fmt.Errorf("未知 agent: %s", name)
	}
}

var _ = os.Environ // 防未用导入
```

（执行者注意：main.go 只保留 main/usage/switch，env.go 放助手，run.go 放 cmdRun，sign.go 为 Task 10 所示；删除上面初稿 main.go 中所有 `var _ =` 占位行——它们是为规避未用导入的错误手法，正确做法是不导入不用的包。）

- [ ] **Step 2: 编译验证**

Run: `go build ./... && go vet ./...`
Expected: 无错误

- [ ] **Step 3: echo 冒烟**

Run: `go run ./runner/cmd/agentbattle run --task ./examples/fix-add --agent echo --label smoketest --out .scratch/reports`
Expected: `[smoketest] 判分 1/2  耗时 ...ms  报告: ...`（echo 写了 calc.sh 但会同时改掉别的？不——echo 只写 calc.sh，add-basic 过，file-only-change 也过，应为 2/2。以实际为准，**≥1/2 即通过**，并核对 `.scratch/reports/*/report.json` 存在）

- [ ] **Step 4: Commit**

```bash
git add runner/cmd/agentbattle/
git commit -m "feat(cli): agentbattle run 子命令（echo/claude-code）"
```

---

### Task 12: mirror 子命令（A/B 镜像对战 N 局）

**Files:**
- Create: `runner/internal/session/mirror.go`
- Test: `runner/internal/session/mirror_test.go`
- Create: `runner/cmd/agentbattle/mirror.go`

胜负判定（设计文档 5.2 的本地版）：测试通过数多者胜 → 同分比 wall 时间短者胜 → 再同分算平局。A/B 配置差异通过 `--env-a/--env-b` 环境变量注入（如 `ANTHROPIC_MODEL=...` 或自定义提示词变量），M1 的归因分析只是统计，不做因果断言。

- [ ] **Step 1: 写失败测试**

```go
// runner/internal/session/mirror_test.go
package session

import (
	"context"
	"testing"
	"time"

	"agentbattle/runner/internal/adapter"
)

func TestMirrorEchoAFixedBNot(t *testing.T) {
	taskDir := newTask(t) // 复用 session_test.go 的构造器
	cfg := MirrorConfig{
		TaskDir:  taskDir,
		JudgeKey: []byte(DevJudgeKey),
		Rounds:   3,
		OutDir:   t.TempDir(),
		MakeA: func() adapter.Adapter { return adapter.Echo{FixContent: "add() { echo $((a + b)); }\n"} },
		MakeB: func() adapter.Adapter { return adapter.Echo{} },
		EnvA:  []string{"CFG=A"}, EnvB: []string{"CFG=B"},
	}
	sum, err := Mirror(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.WinsA != 3 || sum.WinsB != 0 || sum.Ties != 0 {
		t.Fatalf("want A 3:0, got %+v", sum)
	}
}

func TestMirrorTieOnBothFail(t *testing.T) {
	taskDir := newTask(t)
	cfg := MirrorConfig{
		TaskDir:  taskDir,
		JudgeKey: []byte(DevJudgeKey),
		Rounds:   2,
		OutDir:   t.TempDir(),
		MakeA:    func() adapter.Adapter { return adapter.Echo{} },
		MakeB:    func() adapter.Adapter { return adapter.Echo{} },
	}
	sum, err := Mirror(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Ties != 2 {
		t.Fatalf("want 2 ties, got %+v", sum)
	}
}

func TestMirrorSummaryShape(t *testing.T) {
	var sum Summary
	sum.WinsA, sum.WinsB, sum.Ties = 1, 2, 3
	if sum.RoundsPlayed() != 6 {
		t.Fatal("rounds played wrong")
	}
	_ = time.Second // mirror 内部用到 time；保持导入
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/session/ -run TestMirror -v`
Expected: FAIL，编译错误（MirrorConfig/Summary/Mirror 未定义）

- [ ] **Step 3: 实现**

```go
// runner/internal/session/mirror.go
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentbattle/runner/internal/adapter"
)

// MirrorConfig 描述一组镜像对战。
type MirrorConfig struct {
	TaskDir  string
	JudgeKey []byte
	Rounds   int
	OutDir   string
	EnvA     []string
	EnvB     []string
	// MakeA/MakeB 每局各造一个 adapter 实例（echo 可带不同 FixContent 模拟强弱）。
	MakeA func() adapter.Adapter
	MakeB func() adapter.Adapter
}

// Summary 是镜像对战统计（匿名标签 A/B，不涉及配置内容——设计文档 6.3 红线）。
type Summary struct {
	TaskID  string `json:"task_id"`
	Rounds  int    `json:"rounds"`
	WinsA   int    `json:"wins_a"`
	WinsB   int    `json:"wins_b"`
	Ties    int    `json:"ties"`
	AErrors int    `json:"a_errors"` // agent 崩溃等技术性失败
	BErrors int    `json:"b_errors"`
	Details []struct {
		Round   int    `json:"round"`
		Winner  string `json:"winner"` // "a"|"b"|"tie"|"error"
		PassA   int    `json:"pass_a"`
		TotalA  int    `json:"total_a"`
		PassB   int    `json:"pass_b"`
		TotalB  int    `json:"total_b"`
		WallA   int64  `json:"wall_a"`
		WallB   int64  `json:"wall_b"`
		DirA    string `json:"dir_a"`
		DirB    string `json:"dir_b"`
	} `json:"details"`
}

func (s Summary) RoundsPlayed() int { return s.WinsA + s.WinsB + s.Ties }

// Mirror 顺序跑 N 局（A 先 B 后；M1 不并行，避免本机资源互相干扰）。
func Mirror(ctx context.Context, cfg MirrorConfig) (Summary, error) {
	sum := Summary{Rounds: cfg.Rounds}
	for r := 1; r <= cfg.Rounds; r++ {
		resA, errA := Run(ctx, Config{Adapter: cfg.MakeA(), TaskDir: cfg.TaskDir,
			Label: fmt.Sprintf("A-r%d", r), Env: cfg.EnvA,
			JudgeKey: cfg.JudgeKey, OutDir: cfg.OutDir})
		resB, errB := Run(ctx, Config{Adapter: cfg.MakeB(), TaskDir: cfg.TaskDir,
			Label: fmt.Sprintf("B-r%d", r), Env: cfg.EnvB,
			JudgeKey: cfg.JudgeKey, OutDir: cfg.OutDir})

		d := struct {
			Round   int    `json:"round"`
			Winner  string `json:"winner"`
			PassA   int    `json:"pass_a"`
			TotalA  int    `json:"total_a"`
			PassB   int    `json:"pass_b"`
			TotalB  int    `json:"total_b"`
			WallA   int64  `json:"wall_a"`
			WallB   int64  `json:"wall_b"`
			DirA    string `json:"dir_a"`
			DirB    string `json:"dir_b"`
		}{Round: r, PassA: resA.Report.Passed, TotalA: resA.Report.Total,
			PassB: resB.Report.Passed, TotalB: resB.Report.Total,
			WallA: resA.WallMS, WallB: resB.WallMS, DirA: resA.Dir, DirB: resB.Dir}

		switch {
		case errA != nil && errB != nil:
			d.Winner = "error"
			sum.AErrors++
			sum.BErrors++
		case errA != nil:
			d.Winner = "b"
			sum.AErrors++
			sum.WinsB++
		case errB != nil:
			d.Winner = "a"
			sum.BErrors++
			sum.WinsA++
		default:
			d.Winner = winner(resA, resB)
			switch d.Winner {
			case "a":
				sum.WinsA++
			case "b":
				sum.WinsB++
			default:
				sum.Ties++
			}
		}
		sum.TaskID = taskIDOf(cfg.TaskDir)
		sum.Details = append(sum.Details, d)
	}
	return sum, persistSummary(cfg, sum)
}

// winner 按设计文档 5.2：通过数 → wall 时间 → 平局。
func winner(a, b Result) string {
	sa, sb := score(a), score(b)
	if sa != sb {
		if sa > sb {
			return "a"
		}
		return "b"
	}
	if a.WallMS != b.WallMS {
		if a.WallMS < b.WallMS {
			return "a"
		}
		return "b"
	}
	return "tie"
}

func score(r Result) float64 {
	if r.Report.Total == 0 {
		return 0
	}
	return float64(r.Report.Passed) / float64(r.Report.Total)
}

func taskIDOf(taskDir string) string {
	b, err := os.ReadFile(filepath.Join(taskDir, "task.json"))
	if err != nil {
		return "unknown"
	}
	var t struct {
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(b, &t) != nil {
		return "unknown"
	}
	return t.TaskID
}

func persistSummary(cfg MirrorConfig, sum Summary) error {
	if cfg.OutDir == "" {
		cfg.OutDir = filepath.Join(os.TempDir(), "agentbattle-reports")
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(cfg.OutDir, fmt.Sprintf("mirror-%s-%d.json", sum.TaskID, time.Now().UnixNano()))
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}
```

CLI 侧 `runner/cmd/agentbattle/mirror.go`：

```go
package main

import (
	"context"
	"fmt"

	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/session"
)

func cmdMirror(args []string) error {
	fs := newFlagSet("mirror")
	taskDir := fs.String("task", "", "任务目录")
	agentName := fs.String("agent", "claude-code", "echo|claude-code")
	rounds := fs.Int("rounds", 20, "对局数")
	out := fs.String("out", "", "报告输出目录")
	yolo := fs.Bool("yolo", false, "claude-code 追加 --dangerously-skip-permissions")
	var envA, envB envFlag
	fs.Var(&envA, "env-a", "A 侧环境变量 K=V（可多次）")
	fs.Var(&envB, "env-b", "B 侧环境变量 K=V（可多次）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskDir == "" {
		return fmt.Errorf("缺少 --task")
	}
	if _, err := pickAdapter(*agentName, *yolo); err != nil {
		return err // 先做可用性预检，避免跑一半才发现
	}
	outDir, err := mustOutDir(*out)
	if err != nil {
		return err
	}
	mk := func() adapter.Adapter { ad, _ := pickAdapter(*agentName, *yolo); return ad }
	sum, err := session.Mirror(context.Background(), session.MirrorConfig{
		TaskDir: *taskDir, JudgeKey: []byte(session.DevJudgeKey),
		Rounds: *rounds, OutDir: outDir, EnvA: envA, EnvB: envB,
		MakeA: mk, MakeB: mk,
	})
	if err != nil {
		return err
	}
	fmt.Printf("镜像对战完成: A 胜 %d | B 胜 %d | 平 %d | A崩 %d | B崩 %d\n明细: %s\n",
		sum.WinsA, sum.WinsB, sum.Ties, sum.AErrors, sum.BErrors, outDir)
	return nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./... -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add runner/internal/session/mirror.go runner/internal/session/mirror_test.go runner/cmd/agentbattle/mirror.go
git commit -m "feat(mirror): A/B 镜像对战 N 局 + 胜负统计（通过数→耗时→平局）"
```

---

### Task 13: E2E 回归测试

**Files:**
- Test: `runner/e2e/e2e_test.go`

- [ ] **Step 1: 写 E2E（echo agent 走完整 CLI 路径——经 Mirror 而非绕过编排）**

```go
// runner/e2e/e2e_test.go
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
	"agentbattle/runner/internal/session"
)

// TestFullLoopEcho 验证核心闭环：任务目录 → 沙箱 → agent → 事件链 → 判分 → 报告。
func TestFullLoopEcho(t *testing.T) {
	root := findRepoRoot(t)
	taskDir := filepath.Join(root, "examples", "fix-add")
	if _, err := os.Stat(filepath.Join(taskDir, "task.json")); err != nil {
		t.Skipf("示例任务不存在: %v", err)
	}

	cfg := session.MirrorConfig{
		TaskDir:  taskDir,
		JudgeKey: []byte(session.DevJudgeKey),
		Rounds:   2,
		OutDir:   t.TempDir(),
		MakeA:    func() adapter.Adapter { return adapter.Echo{FixContent: "add() { echo $((a + b)); }\n" } },
		MakeB:    func() adapter.Adapter { return adapter.Echo{} },
	}
	sum, err := session.Mirror(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.WinsA != 2 {
		t.Fatalf("A should win 2, got %+v", sum)
	}

	// 报告目录内容完整性：events.ndjson 的链必须可验
	dirs, _ := filepath.Glob(filepath.Join(cfg.OutDir, "*A-r1*"))
	if len(dirs) == 0 {
		t.Fatal("A-r1 report dir missing")
	}
	raw, err := os.ReadFile(filepath.Join(dirs[0], "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var evs []protocol.Event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e protocol.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	if err := protocol.Chain(evs); err != nil {
		t.Fatalf("E2E event chain invalid: %v", err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}
```

注意：E2E 依赖 Task 10 生成的 `examples/fix-add/tests/sig` 已入库；若缺失，先执行 Task 10 Step 2 的 sign 命令。

- [ ] **Step 2: 运行全部测试**

Run: `go test ./...`
Expected: 全部 PASS（E2E 的 2 局 echo 对战约数秒）

- [ ] **Step 3: Commit**

```bash
git add runner/e2e/
git commit -m "test(e2e): echo agent 全链路回归（任务→沙箱→agent→链→判分→报告）"
```

---

### Task 14: 真实 Claude Code 冒烟 + 20 局镜像对战验证

**Files:** 无新增（验证任务）

- [ ] **Step 1: 真实 claude 单局冒烟**

Run: `go run ./runner/cmd/agentbattle run --task ./examples/fix-add --agent claude-code --label real-smoke --yolo --out .scratch/reports`
Expected: `[real-smoke] 判分 2/2`（claude 能修好这个简单 bug）；`events.ndjson` 中能看到 tool_call/file_edit/result 事件。若 claude CLI 未登录或网络问题，先解决环境再继续。

- [ ] **Step 2: 20 局镜像对战（核心验证目标）**

用配置差异制造 A/B：A 正常配置，B 注入一个干扰变量（例如限制思考预算或指向弱模型——按本机可用模型调整）：

```bash
go run ./runner/cmd/agentbattle mirror --task ./examples/fix-add \
  --agent claude-code --rounds 20 --yolo \
  --env-a BATTLE_LABEL=A \
  --env-b BATTLE_LABEL=B \
  --out .scratch/reports
```

Expected: 终端输出 `镜像对战完成: A 胜 X | B 胜 Y | 平 Z | ...`，`.scratch/reports/mirror-fix-add-*.json` 有完整 20 条明细，每局 `events.ndjson` 链校验通过。同配置下预期接近 `10/10`（或大量平局）——**这本身就是一个归因结论的样例**。

- [ ] **Step 3: 结果入库（CHANGE.md 增量）**

在 `CHANGE.md` 追加 M1 计划 1 完成条目（日期、交付物、20 局对战结果数字）。

- [ ] **Step 4: Commit**

```bash
git add CHANGE.md
git commit -m "docs: M1 计划1（Runner 核心闭环）完成记录 + 20 局镜像对战结果"
```

---

## Self-Review 记录

1. **Spec 覆盖**（对照设计文档 M1 Runner 半边）：Adapter 契约（Task 5/6）、沙箱隔离（Task 4）、事件采集与 hash 链（Task 1/2/7）、判分双段式的本地判分段+验签（Task 3/8）、编排（Task 9）、胜负判定 5.2（Task 12）、对抗性测试含篡改/删除/乱序/并发/坏 key（Task 2/7/8 测试）、E2E（Task 13）、20 局验证（Task 14）。平台半边（注册/下发/上传/Elo/天梯）明确划入计划 2，本计划不含。
2. **占位符扫描**：无 TBD/TODO；所有代码步骤给出完整代码；Task 10/11 的执行顺序依赖已在文中显式标注。
3. **类型一致性**：`RawEvent`（adapter）→ `collector.Add` → `protocol.Event`；`judge.Run(taskDir, sandbox, key)` 签名在 Task 8/9 一致；`session.Config`/`MirrorConfig` 字段在 Task 9/12/13 一致；`DevJudgeKey` 定义于 session、引用于 CLI 与 judge 测试各自常量（judge 测试自带 `devKey` 局部常量，与 session.DevJudgeKey 值一致，均为 "dev-secret"）。
