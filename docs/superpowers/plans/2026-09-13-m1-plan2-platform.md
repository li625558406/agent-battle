# M1 计划 2（平台最小功能 + Runner 联网对接）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 平台最小服务端（注册/任务下发/结果上报/Elo 结算/基础天梯）+ Runner 联网对接，验证目标为 Runner 与平台全链路联通、Elo 结算与天梯可见。

**Architecture:** 单 Go module 内新增 `platform/` 子树（与 `runner/` 平级）：`elo`（纯函数评级引擎）→ `store`（SQLite 持久层）→ `api`（net/http 服务）→ `cmd/agentbattle-server`（入口）。Runner 侧新增 `runner/internal/client`（HTTP 客户端 + zip 解包）与 CLI 子命令（register/fetch，mirror 加 `--server` 上报）。mirror 每轮 = 平台一场 match，双侧结果到齐即结算。

**Tech Stack:** Go 1.22+ 标准库 + modernc.org/sqlite（纯 Go，无 cgo，Windows 友好；M1 验证定位，PostgreSQL 划入后续里程碑）+ archive/zip。REST 协议（WebSocket 实时观战属 M3）。前置修复：收官审查遗留 3 项（judge GIT_DIR 过滤 + VerifyDir 导出、mirror 取消路径落盘）。

**约定（全计划适用）：**
- 环境：Windows + git-bash，仓库 `D:\AI\agent-battle`，工作分支从 `m1/runner-core` 新建 `m1/platform`
- 依赖下载需代理：`export http_proxy=socks5://127.0.0.1:10808 https_proxy=socks5://127.0.0.1:10808` 后再 `go get`
- 所有错误信息/注释用简体中文；只用标准库 + modernc.org/sqlite 一个第三方依赖
- 设计红线（项目 CLAUDE.md）：事件流不含文件内容明文；平台永不接触用户私有配置内容
- 每个任务完成标准：`go build ./... && go vet ./... && go test -count=1 -race ./...` 全绿

---

### Task 1: 前置修复——judge GIT_DIR 过滤 + VerifyDir 导出 + Mirror 验签预检

收官审查遗留项 1+2+7。judge 包执行 git（hashGitDiff、测试命令里的 git）时继承宿主环境，用户 shell 里若设了 `GIT_DIR` 会指到宿主仓库；mirror 预检只查 manifest 存在性不验签，坏判分包会让 N 局全记双侧 agent 崩溃。

**Files:**
- Modify: `runner/internal/judge/judge.go`
- Modify: `runner/internal/judge/judge_test.go`
- Modify: `runner/internal/session/mirror.go`
- Test: `runner/internal/session/mirror_test.go`（追加用例）

- [ ] **Step 1: 写失败测试**

先读 `runner/internal/judge/judge.go` 全文，确认现有 `SignDir`/`verifyManifest`/`hashGitDiff`/`runOne` 的实际形态（Task 8/复审后有 baselineSHA 参数与 `.gitignore` 写入逻辑，以现状为准）。在 `judge_test.go` 追加：

```go
// 宿主环境恶意 GIT_DIR 不得影响 judge 内的 git 调用。
func TestRunIgnoresHostGitDirEnv(t *testing.T) {
	dir, sb := newSignedTaskAndSandbox(t) // 若 judge_test 已有等价构造器则复用其模式
	defer os.RemoveAll(dir)
	defer os.RemoveAll(sb)
	t.Setenv("GIT_DIR", filepath.Join(dir, "hostile.git"))
	t.Setenv("GIT_WORK_TREE", dir)
	rep, err := Run(dir, sb, baselineOf(t, sb), devKey)
	if err != nil {
		t.Fatalf("host GIT_DIR leaked into judge: %v", err)
	}
	if rep.Total == 0 {
		t.Fatal("expected tests to run")
	}
}

func TestVerifyDir(t *testing.T) {
	dir := newSignedTaskDir(t) // 复用既有"manifest+sig"构造模式
	defer os.RemoveAll(dir)
	if err := VerifyDir(dir, devKey); err != nil {
		t.Fatalf("valid dir rejected: %v", err)
	}
	// 篡改 manifest 后必须拒绝
	mPath := filepath.Join(dir, "tests", "manifest.json")
	b, _ := os.ReadFile(mPath)
	_ = os.WriteFile(mPath, append(b, []byte(" ")...), 0o644)
	if err := VerifyDir(dir, devKey); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}
```

（`newSignedTaskAndSandbox` 等构造器以 judge_test.go 现有 helper 为准——读文件后按既有模式命名，不硬造第三套。）

在 `mirror_test.go` 追加：

```go
func TestMirrorPreflightRejectsBadManifest(t *testing.T) {
	taskDir := newTask(t)
	// 破坏签名：追加字节到 manifest
	mPath := filepath.Join(taskDir, "tests", "manifest.json")
	b, _ := os.ReadFile(mPath)
	if err := os.WriteFile(mPath, append(b, ' '), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := MirrorConfig{
		TaskDir: taskDir, JudgeKey: []byte(DevJudgeKey), Rounds: 1,
		OutDir: t.TempDir(),
		MakeA:  func() adapter.Adapter { return adapter.Echo{} },
		MakeB:  func() adapter.Adapter { return adapter.Echo{} },
	}
	if _, err := Mirror(context.Background(), cfg); err == nil {
		t.Fatal("bad manifest must fail preflight, not poison N rounds")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/judge/ ./runner/internal/session/ -run 'TestRunIgnoresHostGitDirEnv|TestVerifyDir|TestMirrorPreflight' -v`
Expected: FAIL（`VerifyDir` 未定义；GIT_DIR 测试可能意外通过也可能失败——以实测为准，重点保证实现后稳定通过）

- [ ] **Step 3: 实现**

`judge.go`：
1. 新增包内 `gitEnv()`（与 `sandbox.gitEnv` 同逻辑：`strings.EqualFold` 过滤 `GIT_DIR`/`GIT_WORK_TREE`，保留其余），`hashGitDiff` 的 git 调用改用该 env。注意：**测试命令的 env 不做全量过滤**（agent 需要正常环境变量，如 PATH、代理），只在 `cmd.Env` 组装时以过滤后的环境为基础再追加 `AGENTBATTLE_BASELINE_SHA`——即把现有 `append(os.Environ(), ...)` 改为 `append(gitEnv(), ...)`
2. 导出验证函数（复用现有内部验签逻辑，不重写第二份）：

```go
// VerifyDir 校验任务目录判分包签名与结构完整性，不执行任何测试。
// 供对局编排/平台对接在开赛前预检，避免坏判分包污染整场统计。
func VerifyDir(taskDir string, key []byte) error {
	// 复用现有 verifyManifest/签名比对逻辑：读 tests/manifest.json + tests/sig，
	// HMAC 恒时比较；manifest 缺失、sig 缺失、签名不符均返回带原因的 error
}
```

3. `mirror.go` 预检：manifest 存在性检查升级为 `judge.VerifyDir(cfg.TaskDir, cfg.JudgeKey)`，失败返回 `fmt.Errorf("判分包预检失败: %w", err)`。mirror.go 需新增 import `agentbattle/runner/internal/judge`（确认无 import 环）

- [ ] **Step 4: 运行确认通过**

Run: `go test -count=1 -race ./runner/internal/judge/ ./runner/internal/session/ -v`
Expected: 全 PASS，既有测试无回归

- [ ] **Step 5: Commit**

```bash
git add runner/internal/judge/ runner/internal/session/mirror.go runner/internal/session/mirror_test.go
git commit -m "fix(judge): git 调用过滤宿主 GIT_DIR；导出 VerifyDir 并前移 mirror 验签预检"
```

---

### Task 2: 前置修复——mirror 取消路径

收官审查遗留项 5。ctx 取消发生在局中时：当局双侧 Run 返回 ctx 错误被误记为 agent 崩溃（AErrors/BErrors++），且提前 return 跳过 persistSummary，已完成局的数据丢失。

**Files:**
- Modify: `runner/internal/session/mirror.go`
- Test: `runner/internal/session/mirror_test.go`（追加用例）

- [ ] **Step 1: 写失败测试**

```go
// slowCancelAdapter 持续产事件直到 ctx 取消——复刻"取消打断局中"场景。
type slowCancelAdapter struct{}

func (slowCancelAdapter) Name() string { return "slow-cancel" }
func (slowCancelAdapter) Detect() error {
	return nil
}
func (slowCancelAdapter) Launch(ctx context.Context, cwd, task string, env []string, out chan<- adapter.RawEvent) error {
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- adapter.RawEvent{Type: protocol.EventToolCall, Note: fmt.Sprintf("step-%d", i)}:
		}
	}
}

func TestMirrorCancelMidRound(t *testing.T) {
	taskDir := newTask(t)
	outDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := MirrorConfig{
		TaskDir: taskDir, JudgeKey: []byte(DevJudgeKey), Rounds: 5, OutDir: outDir,
		MakeA: func() adapter.Adapter { return slowCancelAdapter{} },
		MakeB: func() adapter.Adapter { return slowCancelAdapter{} },
	}
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	sum, err := Mirror(ctx, cfg)
	if err == nil {
		t.Fatal("cancelled mirror must return ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if sum.AErrors != 0 || sum.BErrors != 0 {
		t.Fatalf("ctx cancel must not count as agent error: %+v", sum)
	}
	// 部分汇总必须已落盘
	files, _ := filepath.Glob(filepath.Join(outDir, "mirror-*.json"))
	if len(files) == 0 {
		t.Fatal("partial summary not persisted on cancel")
	}
}
```

（测试文件按需补 import `errors`、`fmt`。）

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/session/ -run TestMirrorCancelMidRound -v`
Expected: FAIL（AErrors>0 或未落盘）

- [ ] **Step 3: 实现**

`mirror.go` 的 `Mirror` 循环内，双 Run 完成后、错误归类前：

```go
// ctx 取消/超时导致的错误是"对局被中止"，不是 agent 技术性失败：
// 不计入崩溃统计，落盘部分汇总后原样返回 ctx 错误。
if ctx.Err() != nil || errors.Is(errA, context.Canceled) || errors.Is(errA, context.DeadlineExceeded) {
	if perr := persistSummary(cfg, sum); perr != nil {
		return sum, errors.Join(ctx.Err(), perr)
	}
	return sum, ctx.Err()
}
```

（放在 `case errA != nil && errB != nil:` 之前的 switch 前；`errors.Is(errB, ...)` 同理并入条件。同时修正既有注释"已完成 N 局"的计数说明为含 error 局的实际语义。）

- [ ] **Step 4: 运行确认通过**

Run: `go test -count=1 -race ./runner/internal/session/ -v`
Expected: 全 PASS（含既有 mirror 测试无回归）

- [ ] **Step 5: Commit**

```bash
git add runner/internal/session/mirror.go runner/internal/session/mirror_test.go
git commit -m "fix(mirror): ctx 取消局中不计 agent 崩溃并落盘部分汇总"
```

---

### Task 3: Elo 评级引擎（纯函数）

设计文档 6.1：新 agent 1200 分起；前 10 局定级赛 K=40，之后 K=20。

**Files:**
- Create: `platform/internal/elo/elo.go`
- Test: `platform/internal/elo/elo_test.go`

- [ ] **Step 1: 写失败测试**

```go
// platform/internal/elo/elo_test.go
package elo

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestExpectedSymmetric(t *testing.T) {
	if !almostEqual(Expected(1200, 1200), 0.5) {
		t.Fatal("equal ratings expect 0.5")
	}
	if !almostEqual(Expected(1300, 1200), 1-Expected(1200, 1300)) {
		t.Fatal("expected not symmetric")
	}
	if e := Expected(1400, 1000); e < 0.90 || e > 0.92 {
		t.Fatalf("1000 gap should be ~0.91, got %v", e)
	}
}

func TestUpdateWinLose(t *testing.T) {
	// 双方均未定级（K=40）：1200 vs 1200，A 胜 → A +20，B -20
	na, nb := Update(1200, 1200, 1, 0, 0)
	if !almostEqual(na, 1220) || !almostEqual(nb, 1180) {
		t.Fatalf("want 1220/1180, got %v/%v", na, nb)
	}
}

func TestUpdateTie(t *testing.T) {
	// 平局：强弱分不变于对 1200 vs 1200；强 vs 弱时强方应失分
	na, nb := Update(1200, 1200, 0.5, 5, 5)
	if !almostEqual(na, 1200) || !almostEqual(nb, 1200) {
		t.Fatalf("tie between equals must not change ratings, got %v/%v", na, nb)
	}
	na, nb = Update(1400, 1200, 0.5, 5, 5)
	if na >= 1400 || nb <= 1200 {
		t.Fatalf("tie must favor underdog, got %v/%v", na, nb)
	}
}

func TestKFactorByGames(t *testing.T) {
	if K(0) != 40 || K(9) != 40 {
		t.Fatal("first 10 games are provisional K=40")
	}
	if K(10) != 20 || K(100) != 20 {
		t.Fatal("established players K=20")
	}
	// 定级赛玩家波动更大：同分同胜负，games=0 的分移是 games=50 的两倍
	na1, _ := Update(1200, 1200, 1, 0, 0)
	na2, _ := Update(1200, 1200, 1, 50, 50)
	if math.Abs(na1-1200) != 2*math.Abs(na2-1200) {
		t.Fatalf("K ratio wrong: %v vs %v", na1-1200, na2-1200)
	}
}

func TestUpdateZeroSum(t *testing.T) {
	// 同 K 时零和；跨 K 时各自按自己 K 波动，强方赢弱方收益受 Expected 抑制
	na, nb := Update(1300, 1100, 1, 50, 50)
	if na <= 1300 || nb >= 1100 {
		t.Fatalf("winner must gain, loser must drop: %v/%v", na, nb)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./platform/internal/elo/ -v`
Expected: FAIL（包未定义）

- [ ] **Step 3: 实现**

```go
// Package elo 实现设计文档 6.1 的 ELO 评级：
// 新 agent 1200 分起，前 10 局定级赛 K=40，之后 K=20。
// 纯函数，无 IO，便于平台侧单测与未来按任务类型分榜复用。
package elo

import "math"

const (
	// StartRating 新 agent 初始分。
	StartRating = 1200.0
	// ProvisionalGames 定级赛局数（此前 K=40）。
	ProvisionalGames = 10
)

// Expected 返回 A 在对 B 时的期望得分（0~1）。
func Expected(ra, rb float64) float64 {
	return 1.0 / (1.0 + math.Pow(10, (rb-ra)/400))
}

// K 按已完赛场次返回 K 因子。
func K(games int) float64 {
	if games < ProvisionalGames {
		return 40
	}
	return 20
}

// Update 结算一场对局，返回双方新分。
// scoreA：A 的实际得分（胜 1 / 平 0.5 / 负 0）；
// gamesA/gamesB：结算前双方各自的已完赛场次（决定各自 K）。
func Update(ra, rb float64, scoreA float64, gamesA, gamesB int) (float64, float64) {
	ea := Expected(ra, rb)
	scoreB := 1 - scoreA
	na := ra + K(gamesA)*(scoreA-ea)
	nb := rb + K(gamesB)*(scoreB-(1-ea))
	return na, nb
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test -count=1 -race ./platform/internal/elo/ -v`
Expected: 全 PASS

- [ ] **Step 5: Commit**

```bash
git add platform/internal/elo/
git commit -m "feat(elo): ELO 评级引擎（1200 起，定级赛 K=40，之后 K=20）"
```

---

### Task 4: SQLite 持久层 store

依赖下载（走代理）：`export http_proxy=socks5://127.0.0.1:10808 https_proxy=socks5://127.0.0.1:10808 && go get modernc.org/sqlite`

**Files:**
- Create: `platform/internal/store/store.go`
- Test: `platform/internal/store/store_test.go`
- Modify: `go.mod` / `go.sum`（go get 产物）

- [ ] **Step 1: 写失败测试**

```go
// platform/internal/store/store_test.go
package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAgentUnique(t *testing.T) {
	s := openTest(t)
	a, err := s.CreateAgent("alice")
	if err != nil || a.Token == "" || a.Rating != 1200 {
		t.Fatalf("create agent: %+v err=%v", a, err)
	}
	if _, err := s.CreateAgent("alice"); err == nil {
		t.Fatal("duplicate name must fail")
	}
}

func TestAgentByToken(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("bob")
	got, ok, err := s.AgentByToken(a.Token)
	if err != nil || !ok || got.Name != "bob" {
		t.Fatalf("by token: %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.AgentByToken("nope"); ok {
		t.Fatal("bad token must not resolve")
	}
}

func TestMatchSettleWin(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	mid, err := s.CreateMatch("fix-add", a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 只有一侧结果：未结算
	done, err := s.AddResult(mid, "a", Result{Passed: 2, Total: 2, WallMS: 100, DiffHash: "dh"})
	if err != nil || done {
		t.Fatalf("one side must not settle: done=%v err=%v", done, err)
	}
	done, err = s.AddResult(mid, "b", Result{Passed: 1, Total: 2, WallMS: 50, DiffHash: "dh2"})
	if err != nil || !done {
		t.Fatalf("both sides must settle: done=%v err=%v", done, err)
	}
	// A 通过比例高获胜：新 rating 1220 / 1180（K=40 定级赛）
	ga, _ := s.AgentByID(a.ID)
	gb, _ := s.AgentByID(b.ID)
	if ga.Rating != 1220 || gb.Rating != 1180 {
		t.Fatalf("ratings wrong: %v/%v", ga.Rating, gb.Rating)
	}
	if ga.Wins != 1 || gb.Losses != 1 || ga.Games != 1 {
		t.Fatalf("stats wrong: %+v %+v", ga, gb)
	}
}

func TestMatchSettleTieAndDoubleZero(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A2")
	b, _ := s.CreateAgent("B2")
	mid, _ := s.CreateMatch("fix-add", a.ID, b.ID)
	_, _ = s.AddResult(mid, "a", Result{Passed: 0, Total: 2, WallMS: 10})
	_, err := s.AddResult(mid, "b", Result{Passed: 0, Total: 2, WallMS: 999})
	if err != nil {
		t.Fatal(err)
	}
	ga, _ := s.AgentByID(a.ID)
	gb, _ := s.AgentByID(b.ID)
	// 双方 0 通过 → 平局（与 runner 侧 winner 规则一致）
	if ga.Rating != 1200 || gb.Rating != 1200 || ga.Ties != 1 {
		t.Fatalf("double-zero must tie: %+v %+v", ga, gb)
	}
}

func TestLadderOrder(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("winner")
	b, _ := s.CreateAgent("loser")
	mid, _ := s.CreateMatch("fix-add", a.ID, b.ID)
	_, _ = s.AddResult(mid, "a", Result{Passed: 2, Total: 2, WallMS: 100})
	_, _ = s.AddResult(mid, "b", Result{Passed: 0, Total: 2, WallMS: 100})
	rows, err := s.Ladder()
	if err != nil || len(rows) != 2 {
		t.Fatalf("ladder: %v err=%v", rows, err)
	}
	if rows[0].Name != "winner" || rows[0].Rating <= rows[1].Rating {
		t.Fatalf("ladder order wrong: %+v", rows)
	}
}

func TestAddResultRejectsBadSideAndDuplicate(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A3")
	b, _ := s.CreateAgent("B3")
	mid, _ := s.CreateMatch("fix-add", a.ID, b.ID)
	if err := s.AddResultSide(mid, "c", Result{}); err == nil {
		t.Fatal("bad side must fail")
	}
	if err := s.AddResultSide(mid, "a", Result{Total: 1}); err != nil {
		t.Fatalf("first a: %v", err)
	}
	if err := s.AddResultSide(mid, "a", Result{Total: 1}); err == nil {
		t.Fatal("duplicate side must fail")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./platform/internal/store/ -v`
Expected: FAIL（包未定义）

- [ ] **Step 3: 实现**

```go
// Package store 是平台 M1 的 SQLite 持久层（modernc.org/sqlite 纯 Go 驱动）。
// M1 验证定位；PostgreSQL 迁移在后续里程碑，收敛在本包内替换。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"agentbattle/platform/internal/elo"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS agents (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	name     TEXT UNIQUE NOT NULL,
	token    TEXT UNIQUE NOT NULL,
	rating   REAL NOT NULL DEFAULT 1200,
	games    INTEGER NOT NULL DEFAULT 0,
	wins     INTEGER NOT NULL DEFAULT 0,
	losses   INTEGER NOT NULL DEFAULT 0,
	ties     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS matches (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    TEXT NOT NULL,
	agent_a    INTEGER NOT NULL REFERENCES agents(id),
	agent_b    INTEGER NOT NULL REFERENCES agents(id),
	status     TEXT NOT NULL DEFAULT 'pending',
	winner     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	settled_at INTEGER
);
CREATE TABLE IF NOT EXISTS results (
	match_id  INTEGER NOT NULL REFERENCES matches(id),
	side      TEXT NOT NULL CHECK(side IN ('a','b')),
	passed    INTEGER NOT NULL,
	total     INTEGER NOT NULL,
	wall_ms   INTEGER NOT NULL,
	diff_hash TEXT NOT NULL DEFAULT '',
	events_gz BLOB,
	PRIMARY KEY (match_id, side)
);`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Agent struct {
	ID                 int64
	Name, Token        string
	Rating             float64
	Games, Wins, Losses, Ties int
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func (s *Store) CreateAgent(name string) (Agent, error) {
	tok := newToken()
	res, err := s.db.Exec(
		`INSERT INTO agents (name, token, created_at) VALUES (?, ?, ?)`,
		name, tok, time.Now().Unix())
	if err != nil {
		return Agent{}, fmt.Errorf("agent 名已存在或写入失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return Agent{ID: id, Name: name, Token: tok, Rating: elo.StartRating}, nil
}

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.Name, &a.Token, &a.Rating, &a.Games, &a.Wins, &a.Losses, &a.Ties)
	return a, err
}

const agentCols = `id, name, token, rating, games, wins, losses, ties`

func (s *Store) AgentByToken(token string) (Agent, bool, error) {
	a, err := scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE token = ?`, token))
	if err == sql.ErrNoRows {
		return Agent{}, false, nil
	}
	return a, err == nil, err
}

func (s *Store) AgentByID(id int64) (Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

func (s *Store) CreateMatch(taskID string, agentA, agentB int64) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO matches (task_id, agent_a, agent_b, created_at) VALUES (?, ?, ?, ?)`,
		taskID, agentA, agentB, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Result 是一侧的对局结果（events_gz 为 NDJSON 事件流 gzip 压缩，可空）。
type Result struct {
	Passed, Total int
	WallMS        int64
	DiffHash      string
	EventsGZ      []byte
}

// AddResultSide 记录一侧结果；双侧到齐时结算 Elo 并更新统计。
// 返回该次写入后对局是否已结算。
func (s *Store) AddResult(matchID int64, side string, r Result) (bool, error) {
	if side != "a" && side != "b" {
		return false, fmt.Errorf("非法 side: %q", side)
	}
	if _, err := s.db.Exec(
		`INSERT INTO results (match_id, side, passed, total, wall_ms, diff_hash, events_gz)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		matchID, side, r.Passed, r.Total, r.WallMS, r.DiffHash, r.EventsGZ); err != nil {
		return false, fmt.Errorf("写入结果（重复提交同一侧会被主键拒绝）: %w", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM results WHERE match_id = ?`, matchID).Scan(&n); err != nil {
		return false, err
	}
	if n < 2 {
		return false, nil
	}
	return true, s.settle(matchID)
}

// AddResultSide 仅写入不结算（测试用）。
func (s *Store) AddResultSide(matchID int64, side string, r Result) error {
	_, err := s.AddResult(matchID, side, r)
	if err != nil {
		return err
	}
	return nil
}

// settle 复算胜负（与 runner 侧 session/mirror.go 的 winner 规则一致：
// 通过比例 → wall 时间 → 平局；双零平局）并落 Elo 与统计。
// 注意：winner 逻辑在 runner（agentbattle/runner/internal，跨 internal 边界不可导入）
// 与本包各有一份，规则变更必须双侧同步——已在两处注释互相指向。
func (s *Store) settle(matchID int64) error {
	var pa, pb, ta, tb int
	var wa, wb int64
	if err := s.db.QueryRow(
		`SELECT side, passed, total, wall_ms FROM results WHERE match_id = ?`, matchID,
	).Err(); err == nil {
	}
	rows, err := s.db.Query(`SELECT side, passed, total, wall_ms FROM results WHERE match_id = ?`, matchID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var side string
		var p, t int
		var w int64
		if err := rows.Scan(&side, &p, &t, &w); err != nil {
			return err
		}
		if side == "a" {
			pa, ta, wa = p, t, w
		} else {
			pb, tb, wb = p, t, w
		}
	}
	var taskID string
	var ida, idb int64
	if err := s.db.QueryRow(
		`SELECT task_id, agent_a, agent_b FROM matches WHERE id = ?`, matchID,
	).Scan(&taskID, &ida, &idb); err != nil {
		return err
	}
	agA, err := s.AgentByID(ida)
	if err != nil {
		return err
	}
	agB, err := s.AgentByID(idb)
	if err != nil {
		return err
	}

	winner := winnerOf(pa, ta, wa, pb, tb, wb)
	scoreA := 0.5
	switch winner {
	case "a":
		scoreA = 1
	case "b":
		scoreA = 0
	}
	na, nb := elo.Update(agA.Rating, agB.Rating, scoreA, agA.Games, agB.Games)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	bump := func(id int64, rating float64, won bool, tie bool) error {
		res := "losses"
		if tie {
			res = "ties"
		} else if won {
			res = "wins"
		}
		_, err := tx.Exec(
			`UPDATE agents SET rating = ?, games = games + 1,
			 wins = wins + (CASE WHEN ? = 'wins' THEN 1 ELSE 0 END),
			 losses = losses + (CASE WHEN ? = 'losses' THEN 1 ELSE 0 END),
			 ties = ties + (CASE WHEN ? = 'ties' THEN 1 ELSE 0 END)
			 WHERE id = ?`, rating, res, res, res, id)
		return err
	}
	if err := bump(ida, na, winner == "a", winner == "tie"); err != nil {
		return err
	}
	if err := bump(idb, nb, winner == "b", winner == "tie"); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE matches SET status='done', winner=?, settled_at=? WHERE id=?`,
		winner, time.Now().Unix(), matchID); err != nil {
		return err
	}
	return tx.Commit()
}

// winnerOf：通过比例高者胜 → 同比例 wall 短者胜 → 平局。双零直接平局
// （失败的耗时没有竞速信号，与 runner 侧判定一致）。
func winnerOf(pa, ta int, wa int64, pb, tb int, wb int64) string {
	sa, sb := ratio(pa, ta), ratio(pb, tb)
	if sa != sb {
		if sa > sb {
			return "a"
		}
		return "b"
	}
	if wa != wb {
		if wa < wb {
			return "a"
		}
		return "b"
	}
	return "tie"
}

func ratio(p, t int) float64 {
	if t == 0 {
		return 0
	}
	return float64(p) / float64(t)
}

func (s *Store) Ladder() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY rating DESC, games DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
```

（实现时以编译为准做微调：`AddResultSide` 的重复写入路径要保证 duplicate 主键错误向上冒泡；`settle` 开头误留的 `QueryRow().Err()` 死代码不要照抄——写干净的循环即可。）

- [ ] **Step 4: 运行确认通过**

Run: `go test -count=1 -race ./platform/internal/store/ -v`
Expected: 全 PASS

- [ ] **Step 5: Commit**

```bash
git add platform/internal/store/ go.mod go.sum
git commit -m "feat(store): SQLite 持久层（agent/match/result + Elo 结算 + 天梯）"
```

---

### Task 5: HTTP API 服务 + server 入口

**Files:**
- Create: `platform/internal/api/api.go`
- Test: `platform/internal/api/api_test.go`
- Create: `platform/cmd/agentbattle-server/main.go`

API 契约（REST，JSON，认证用 `X-Token` 头）：
- `POST /api/agents` `{"name"}` → `201 {"id","name","token"}`；重名 409
- `POST /api/matches`（auth）`{"task_id","agent_a","agent_b"}`（后两者为 name）→ `201 {"match_id","judge_key"}`；任务不存在 400、agent 不存在 400
- `POST /api/matches/{id}/results`（auth，token 必须属于该 match 的 a 或 b）`{"side","passed","total","wall_ms","diff_hash","events_gz_base64"}` → `200 {"status":"waiting"|"done","winner","rating_a","rating_b"}`
- `GET /api/ladder` → `200 {"ladder":[{"name","rating","games","wins","losses","ties"}]}`
- `GET /api/tasks/{id}/bundle` → `200 application/zip`（任务目录整体打包，含 tests/sig）

- [ ] **Step 1: 写失败测试**

```go
// platform/internal/api/api_test.go
package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentbattle/platform/internal/store"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tasksDir := t.TempDir()
	// 伪造一个最小任务目录
	os.MkdirAll(filepath.Join(tasksDir, "demo", "seed"), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, "demo", "tests"), 0o755)
	os.WriteFile(filepath.Join(tasksDir, "demo", "task.json"),
		[]byte(`{"task_id":"demo","name":"d","description":"x","timeout_sec":60}`), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "demo", "tests", "manifest.json"),
		[]byte(`{"task_id":"demo","tests":[]}`), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "demo", "tests", "sig"), []byte("sig"), 0o644)

	srv := httptest.NewServer(New(st, tasksDir, []byte("dev-secret")))
	t.Cleanup(srv.Close)
	return srv, st
}

func do(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		json.Unmarshal(b, &m)
	}
	return resp, m
}

func TestRegisterAgent(t *testing.T) {
	srv, _ := newServer(t)
	resp, m := do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": "alice"})
	if resp.StatusCode != 201 || m["token"] == "" {
		t.Fatalf("register: %d %v", resp.StatusCode, m)
	}
	resp, _ = do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": "alice"})
	if resp.StatusCode != 409 {
		t.Fatalf("dup name must 409, got %d", resp.StatusCode)
	}
	resp, _ = do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": ""})
	if resp.StatusCode != 400 {
		t.Fatalf("empty name must 400, got %d", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := do(t, "POST", srv.URL+"/api/matches", "", map[string]any{"task_id": "demo"})
	if resp.StatusCode != 401 {
		t.Fatalf("no token must 401, got %d", resp.StatusCode)
	}
	resp, _ = do(t, "POST", srv.URL+"/api/matches", "bogus", map[string]any{"task_id": "demo"})
	if resp.StatusCode != 401 {
		t.Fatalf("bad token must 401, got %d", resp.StatusCode)
	}
}

func TestMatchFlow(t *testing.T) {
	srv, _ := newServer(t)
	_, ma := do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": "A"})
	_, mb := do(t, "POST", srv.URL+"/api/agents", "", map[string]any{"name": "B"})
	tokA := ma["token"].(string)
	tokB := mb["token"].(string)

	// 不存在的任务
	resp, _ := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "nope", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown task must 400, got %d", resp.StatusCode)
	}
	// 正常创建
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 201 || m["judge_key"] == "" {
		t.Fatalf("create match: %d %v", resp.StatusCode, m)
	}
	mid := int64(m["match_id"].(float64))
	// 无关 token 上报 → 403
	resp, _ = do(t, "POST", srv.URL+fmtMatch(mid), "intruder",
		map[string]any{"side": "a", "passed": 1, "total": 1})
	if resp.StatusCode != 403 {
		t.Fatalf("foreign token must 403, got %d", resp.StatusCode)
	}
	// A 上报（等待）
	resp, m = do(t, "POST", srv.URL+fmtMatch(mid), tokA,
		map[string]any{"side": "a", "passed": 2, "total": 2, "wall_ms": 100, "diff_hash": "d1"})
	if resp.StatusCode != 200 || m["status"] != "waiting" {
		t.Fatalf("first result: %d %v", resp.StatusCode, m)
	}
	// B 上报（结算，A 胜）
	resp, m = do(t, "POST", srv.URL+fmtMatch(mid), tokB,
		map[string]any{"side": "b", "passed": 1, "total": 2, "wall_ms": 10, "diff_hash": "d2"})
	if resp.StatusCode != 200 || m["status"] != "done" || m["winner"] != "a" {
		t.Fatalf("settle: %d %v", resp.StatusCode, m)
	}
	if m["rating_a"].(float64) != 1220 || m["rating_b"].(float64) != 1180 {
		t.Fatalf("ratings: %v", m)
	}
	// 天梯
	_, m = do(t, "GET", srv.URL+"/api/ladder", "", nil)
	lad := m["ladder"].([]any)
	if len(lad) != 2 || lad[0].(map[string]any)["name"] != "A" {
		t.Fatalf("ladder: %v", m)
	}
}

func fmtMatch(id int64) string { return "/api/matches/" + itoa(id) + "/results" }

func itoa(i int64) string { return strings.TrimSpace(strings.ReplaceAll(fmtInt(i), " ", "")) }

func fmtInt(i int64) string { return strconvI(i) }
```

（测试文件顶部补 `"fmt"`、`"strconv"`，`itoa` 直接用 `strconv.FormatInt(i, 10)` 实现——上面三个辅助函数写成一个即可，不要照抄冗余链。）

再补任务包下载测试：

```go
func TestTaskBundle(t *testing.T) {
	srv, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/api/tasks/demo/bundle")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("bundle: %v %d", err, resp.StatusCode)
	}
	defer resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(mustRead(t, resp.Body)), int64(0))
	// zip.NewReader 需要大小：用 io.ReadAll 后 bytes.NewReader(b), int64(len(b))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["task.json"] || !names["tests/sig"] || !names["tests/manifest.json"] {
		t.Fatalf("bundle missing files: %v", names)
	}
	resp2, _ := http.Get(srv.URL + "/api/tasks/nope/bundle")
	if resp2.StatusCode != 404 {
		t.Fatalf("unknown task must 404, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./platform/internal/api/ -v`
Expected: FAIL（包未定义）

- [ ] **Step 3: 实现**

```go
// Package api 是平台 M1 的 HTTP 服务：注册 / 对局创建 / 结果上报与 Elo 结算 / 天梯 / 任务包分发。
package api

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"agentbattle/platform/internal/store"
)

type Server struct {
	St       *store.Store
	TasksDir string // 任务目录根（每个子目录 = 一个任务：task.json + seed/ + tests/ 含 sig）
	JudgeKey []byte // M1 固定开发密钥；平台化后按局随机下发
}

func New(st *store.Store, tasksDir string, judgeKey []byte) http.Handler {
	s := &Server{St: st, TasksDir: tasksDir, JudgeKey: judgeKey}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents", s.handleRegister)
	mux.HandleFunc("POST /api/matches", s.auth(s.handleCreateMatch))
	mux.HandleFunc("POST /api/matches/{id}/results", s.auth(s.handleResult))
	mux.HandleFunc("GET /api/ladder", s.handleLadder)
	mux.HandleFunc("GET /api/tasks/{id}/bundle", s.handleBundle)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) auth(next func(w http.ResponseWriter, r *http.Request, ag store.Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ag, ok, err := s.St.AgentByToken(r.Header.Get("X-Token"))
		if err != nil || !ok {
			writeErr(w, http.StatusUnauthorized, "无效 token")
			return
		}
		next(w, r, ag)
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "body 需为 {\"name\":\"非空\"}")
		return
	}
	ag, err := s.St.CreateAgent(strings.TrimSpace(body.Name))
	if err != nil {
		writeErr(w, http.StatusConflict, "agent 名已存在: "+body.Name)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": ag.ID, "name": ag.Name, "token": ag.Token})
}

func (s *Server) handleCreateMatch(w http.ResponseWriter, r *http.Request, _ store.Agent) {
	var body struct {
		TaskID string `json:"task_id"`
		AgentA string `json:"agent_a"`
		AgentB string `json:"agent_b"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.TaskID == "" || body.AgentA == "" || body.AgentB == "" {
		writeErr(w, http.StatusBadRequest, "body 需为 {task_id, agent_a, agent_b}")
		return
	}
	if _, err := os.Stat(filepath.Join(s.TasksDir, body.TaskID, "task.json")); err != nil {
		writeErr(w, http.StatusBadRequest, "任务不存在: "+body.TaskID)
		return
	}
	agA, ok, err := s.St.AgentByToken("") // 占位：应改为 AgentByName——见下方说明
	_ = agA
	if err != nil || !ok {
		// 实现时补 store.AgentByName 查询
	}
	writeJSON(w, http.StatusCreated, map[string]any{"match_id": 0, "judge_key": string(s.JudgeKey)})
}
```

（上面 handleCreateMatch 是**结构示意**，实现时必须完整：给 store 补 `AgentByName(name string) (Agent, bool, error)`，两个 name 都解析成功后 `CreateMatch`，任何一步失败 400；match_id 用真实 `LastInsertId`。不要把占位代码提交进仓库。）

```go
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, ag store.Agent) {
	idStr := r.PathValue("id")
	var mid int64
	if _, err := fmt.Sscanf(idStr, "%d", &mid); err != nil {
		writeErr(w, http.StatusBadRequest, "match id 非法")
		return
	}
	// token 必须属于该 match 双方之一（防无关方篡改结果）
	partOf, err := s.agentInMatch(ag.ID, mid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !partOf {
		writeErr(w, http.StatusForbidden, "token 不属于该对局双方")
		return
	}
	var body struct {
		Side         string `json:"side"`
		Passed, Total int
		WallMS       int64  `json:"wall_ms"`
		DiffHash     string `json:"diff_hash"`
		EventsGZB64  string `json:"events_gz_base64"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		(body.Side != "a" && body.Side != "b") {
		writeErr(w, http.StatusBadRequest, "body 需为 {side(a|b), passed, total, wall_ms, diff_hash, events_gz_base64}")
		return
	}
	var gz []byte
	if body.EventsGZB64 != "" {
		if b, err := base64.StdEncoding.DecodeString(body.EventsGZB64); err == nil {
			gz = b
		}
	}
	done, err := s.St.AddResult(mid, body.Side, store.Result{
		Passed: body.Passed, Total: body.Total, WallMS: body.WallMS,
		DiffHash: body.DiffHash, EventsGZ: gz,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := map[string]any{"status": "waiting"}
	if done {
		// 重读双方最新分与胜者
		winner, ra, rb, err := s.matchOutcome(mid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = map[string]any{"status": "done", "winner": winner, "rating_a": ra, "rating_b": rb}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLadder(w http.ResponseWriter, r *http.Request) {
	rows, err := s.St.Ladder()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		Name   string  `json:"name"`
		Rating float64 `json:"rating"`
		Games  int     `json:"games"`
		Wins   int     `json:"wins"`
		Losses int     `json:"losses"`
		Ties   int     `json:"ties"`
	}
	lad := make([]row, 0, len(rows))
	for _, a := range rows {
		lad = append(lad, row{a.Name, a.Rating, a.Games, a.Wins, a.Losses, a.Ties})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ladder": lad})
}

func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	root := filepath.Join(s.TasksDir, id)
	if _, err := os.Stat(filepath.Join(root, "task.json")); err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在: "+id)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		f, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(f, src)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打包失败: "+err.Error())
		return
	}
	zw.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}
```

`agentInMatch` / `matchOutcome` 为包内辅助：查 matches 表确认 ag.ID ∈ {agent_a, agent_b}（store 补一个 `MatchByID(id) (taskID string, aID, bID int64, status, winner string, err error)`）；结算后读双方 rating 返回。

server 入口 `platform/cmd/agentbattle-server/main.go`：

```go
// agentbattle-server：平台 M1 最小服务端。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"agentbattle/platform/internal/api"
	"agentbattle/platform/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	tasksDir := flag.String("tasks", "./examples", "任务目录根")
	dbPath := flag.String("store", "agentbattle.db", "SQLite 数据库路径")
	judgeKey := flag.String("judge-key", "dev-secret", "判分包 HMAC 密钥（M1 开发用）")
	flag.Parse()
	if _, err := os.Stat(filepath.Join(*tasksDir, "fix-add", "task.json")); err != nil {
		log.Printf("警告: 任务目录不含 fix-add (%v)", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	h := api.New(st, *tasksDir, []byte(*judgeKey))
	fmt.Printf("agentbattle-server 监听 %s（任务根 %s，库 %s）\n", *addr, *tasksDir, *dbPath)
	log.Fatal(http.ListenAndServe(*addr, h))
}
```

（补 `"path/filepath"` import。）

- [ ] **Step 4: 运行确认通过**

Run: `go test -count=1 -race ./platform/... -v && go build ./... && go vet ./...`
Expected: 全 PASS

- [ ] **Step 5: Commit**

```bash
git add platform/ go.mod go.sum
git commit -m "feat(api): 平台 M1 服务端（注册/对局/结果上报 Elo 结算/天梯/任务包 zip 分发）"
```

---

### Task 6: Runner 侧 HTTP 客户端 + zip 解包

**Files:**
- Create: `runner/internal/client/client.go`
- Test: `runner/internal/client/client_test.go`

- [ ] **Step 1: 写失败测试**

用真实 `api.New` 起 httptest 服务做集成式单测（跨 internal 目录不可导入 api——api 在 platform/internal，runner 在 runner/internal：**同 module 下 `internal` 可见性以目录子树为界**，`runner/internal/client` 不能 import `platform/internal/api`。因此测试用 `httptest.NewServer` + 手写 stub handler 验证协议，另在 Task 8 做跨端 E2E。stub 按上方 API 契约返回固定 JSON）：

```go
// runner/internal/client/client_test.go
package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterAndMatchFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/agents" && r.Method == "POST":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":1,"name":"A","token":"tok-1"}`))
		case r.URL.Path == "/api/matches" && r.Method == "POST":
			if r.Header.Get("X-Token") != "tok-1" {
				w.WriteHeader(401)
				return
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"match_id":7,"judge_key":"dev-secret"}`))
		case r.URL.Path == "/api/matches/7/results" && r.Method == "POST":
			w.Write([]byte(`{"status":"done","winner":"a","rating_a":1220,"rating_b":1180}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	id, tok, err := c.Register("A")
	if err != nil || id != 1 || tok != "tok-1" {
		t.Fatalf("register: %v %d %s", err, id, tok)
	}
	mid, key, err := c.CreateMatch("tok-1", "fix-add", "A", "B")
	if err != nil || mid != 7 || key != "dev-secret" {
		t.Fatalf("create match: %v %d %s", err, mid, key)
	}
	settle, err := c.UploadResult("tok-1", 7, ResultIn{Side: "a", Passed: 2, Total: 2, WallMS: 100})
	if err != nil || settle.Status != "done" || settle.Winner != "a" || settle.RatingA != 1220 {
		t.Fatalf("upload: %+v err=%v", settle, err)
	}
}

func TestErrorSurfacing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"error":"agent 名已存在: A"}`))
	}))
	defer srv.Close()
	_, _, err := New(srv.URL).Register("A")
	if err == nil || !strings.Contains(err.Error(), "agent 名已存在") {
		t.Fatalf("server error must surface: %v", err)
	}
}

func TestExtractBundleRejectsTraversal(t *testing.T) {
	// 构造含 ../ 路径的恶意 zip
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../../evil.txt")
	w.Write([]byte("pwned"))
	zw.Close()
	dir := t.TempDir()
	if err := ExtractBundle(buf.Bytes(), dir); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.txt")); err == nil {
		t.Fatal("file escaped extraction dir")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatal("file escaped to parent")
	}
}

func TestExtractBundleRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"task.json":          `{"task_id":"demo"}`,
		"seed/calc.sh":       "add() {}",
		"tests/manifest.json": `{"task_id":"demo"}`,
		"tests/sig":          "s",
	} {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	dir := t.TempDir()
	if err := ExtractBundle(buf.Bytes(), dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "seed", "calc.sh"))
	if err != nil || string(b) != "add() {}" {
		t.Fatalf("roundtrip: %v %s", err, b)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./runner/internal/client/ -v`
Expected: FAIL（包未定义）

- [ ] **Step 3: 实现**

```go
// Package client 是 Runner 对接平台的 HTTP 客户端（REST，X-Token 认证）。
package client

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"agentbattle/protocol"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(method, path, token string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("请求平台失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("平台返回 %d: %s", resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Register(name string) (id int64, token string, err error) {
	var resp struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err = c.do("POST", "/api/agents", "", map[string]string{"name": name}, &resp); err != nil {
		return 0, "", err
	}
	return resp.ID, resp.Token, nil
}

func (c *Client) CreateMatch(token, taskID, agentA, agentB string) (matchID int64, judgeKey string, err error) {
	var resp struct {
		MatchID  int64  `json:"match_id"`
		JudgeKey string `json:"judge_key"`
	}
	body := map[string]string{"task_id": taskID, "agent_a": agentA, "agent_b": agentB}
	if err = c.do("POST", "/api/matches", token, body, &resp); err != nil {
		return 0, "", err
	}
	return resp.MatchID, resp.JudgeKey, nil
}

// ResultIn 上报给平台的一侧结果。
type ResultIn struct {
	Side         string
	Passed, Total int
	WallMS       int64
	DiffHash     string
	EventsGZ     []byte
}

type Settle struct {
	Status          string  `json:"status"`
	Winner          string  `json:"winner"`
	RatingA, RatingB float64 `json:"rating_a"`
}

func (c *Client) UploadResult(token string, matchID int64, r ResultIn) (Settle, error) {
	var resp Settle
	body := map[string]any{
		"side": r.Side, "passed": r.Passed, "total": r.Total,
		"wall_ms": r.WallMS, "diff_hash": r.DiffHash,
	}
	if len(r.EventsGZ) > 0 {
		body["events_gz_base64"] = base64.StdEncoding.EncodeToString(r.EventsGZ)
	}
	err := c.do("POST", fmt.Sprintf("/api/matches/%d/results", matchID), token, body, &resp)
	return resp, err
}

type LadderRow struct {
	Name   string  `json:"name"`
	Rating float64 `json:"rating"`
	Games  int     `json:"games"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	Ties   int     `json:"ties"`
}

func (c *Client) Ladder() ([]LadderRow, error) {
	var resp struct {
		Ladder []LadderRow `json:"ladder"`
	}
	if err := c.do("GET", "/api/ladder", "", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Ladder, nil
}

func (c *Client) FetchBundle(taskID string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.Base+"/api/tasks/"+path.Escape? /* 用 url.PathEscape(taskID) */ +"/bundle", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("拉取任务包 %d: %s", resp.StatusCode, string(b))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20)) // 上限 256MB 防失控响应
}

// ExtractBundle 解压任务包到 destDir，拒绝路径穿越（zip slip）。
func ExtractBundle(zipBytes []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("任务包不是合法 zip: %w", err)
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("任务包含非法路径: %q", f.Name)
		}
		target := filepath.Join(absDest, name)
		if !strings.HasPrefix(target, absDest+string(filepath.Separator)) && target != absDest {
			return fmt.Errorf("任务包含非法路径: %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		// 单文件上限 64MB，防 zip 炸弹
		n, err := io.Copy(out, io.LimitReader(rc, 64<<20))
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		if n == 64<<20 {
			return fmt.Errorf("任务包单文件超限: %q", f.Name)
		}
	}
	return nil
}

// GzipEvents 把事件流序列化为 NDJSON 并 gzip 压缩（上报用；不含文件内容明文——
// 事件仅路径/操作类型，隐私红线见项目 CLAUDE.md）。
func GzipEvents(events []protocol.Event) ([]byte, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
```

（`FetchBundle` 里 URL 拼接用 `url.PathEscape(taskID)`；import `"net/url"`。上面伪码占位行不要照抄。）

- [ ] **Step 4: 运行确认通过**

Run: `go test -count=1 -race ./runner/internal/client/ -v`
Expected: 全 PASS

- [ ] **Step 5: Commit**

```bash
git add runner/internal/client/
git commit -m "feat(client): Runner 平台对接客户端（注册/对局/上报/天梯/任务包解压防 zip slip）"
```

---

### Task 7: CLI 对接（register/fetch 子命令 + mirror --server 上报）

**Files:**
- Modify: `runner/cmd/agentbattle/main.go`（switch 加 register/fetch）
- Create: `runner/cmd/agentbattle/register.go`
- Create: `runner/cmd/agentbattle/fetch.go`
- Modify: `runner/cmd/agentbattle/mirror.go`（--server 模式）
- Modify: `runner/internal/session/mirror.go`（MirrorConfig 加 Report 钩子）
- Modify: `runner/internal/session/mirror_test.go`（钩子用例）

- [ ] **Step 1: mirror 加每轮回调**

`MirrorConfig` 追加字段：

```go
// Report 每轮结束（双 Run 返回后、统计归类后）回调；返回 error 立即中止
// 并原样上抛（联网上报失败即停，不静默丢局）。nil = 纯本地模式。
Report func(ctx context.Context, round int, resA, resB Result, errA, errB error) error
```

`Mirror` 循环末尾（`sum.Details = append(...)` 之后）：

```go
if cfg.Report != nil {
	if err := cfg.Report(ctx, r, resA, resB, errA, errB); err != nil {
		return sum, fmt.Errorf("第 %d 轮上报失败: %w", r, err)
	}
}
```

测试（mirror_test.go 追加）：

```go
func TestMirrorReportHook(t *testing.T) {
	taskDir := newTask(t)
	var rounds []int
	cfg := MirrorConfig{
		TaskDir: taskDir, JudgeKey: []byte(DevJudgeKey), Rounds: 2, OutDir: t.TempDir(),
		MakeA: func() adapter.Adapter { return adapter.Echo{FixContent: "add() { echo $(( $1 + $2 )); }\n"} },
		MakeB: func() adapter.Adapter { return adapter.Echo{} },
		Report: func(_ context.Context, r int, a, b Result, ea, eb error) error {
			rounds = append(rounds, r)
			if a.Report.Passed != 2 || b.Report.Passed != 1 || ea != nil || eb != nil {
				t.Fatalf("round %d results wrong: %+v %+v", r, a.Report, b.Report)
			}
			return nil
		},
	}
	if _, err := Mirror(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[0] != 1 || rounds[1] != 2 {
		t.Fatalf("hook rounds: %v", rounds)
	}
}
```

先写测试跑红（Report 字段未定义）再实现。

- [ ] **Step 2: register / fetch 子命令**

`runner/cmd/agentbattle/register.go`：

```go
package main

import (
	"flag"
	"fmt"

	"agentbattle/runner/internal/client"
)

// cmdRegister: agentbattle register --server URL --name X → 打印 token（M1 不落盘，用户自行保存）
func cmdRegister(args []string) error {
	fs := newFlagSet("register")
	server := fs.String("server", "", "平台地址，如 http://127.0.0.1:8080")
	name := fs.String("name", "", "agent 名称（天梯显示用）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" || *name == "" {
		return fmt.Errorf("缺少 --server 或 --name")
	}
	id, token, err := client.New(*server).Register(*name)
	if err != nil {
		return err
	}
	fmt.Printf("注册成功: %s (id=%d)\ntoken: %s\n请妥善保存 token（上传结果时使用）。\n", *name, id, token)
	return nil
}
```

`runner/cmd/agentbattle/fetch.go`：

```go
package main

import (
	"flag"
	"fmt"
	"path/filepath"

	"agentbattle/runner/internal/client"
)

// cmdFetch: agentbattle fetch --server URL --task fix-add --out DIR
func cmdFetch(args []string) error {
	fs := newFlagSet("fetch")
	server := fs.String("server", "", "平台地址")
	taskID := fs.String("task", "", "任务 ID")
	out := fs.String("out", "", "解压目标目录")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" || *taskID == "" || *out == "" {
		return fmt.Errorf("缺少 --server / --task / --out")
	}
	bundle, err := client.New(*server).FetchBundle(*taskID)
	if err != nil {
		return err
	}
	dest, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if err := client.ExtractBundle(bundle, dest); err != nil {
		return err
	}
	fmt.Printf("任务包 %s 已解压到 %s\n", *taskID, dest)
	return nil
}
```

`main.go` switch 追加 `case "register": err = cmdRegister(os.Args[2:])` 与 `case "fetch": err = cmdFetch(os.Args[2:])`，usage 文本同步加两行。

- [ ] **Step 3: mirror --server 上报模式**

`cmdMirror` 追加 flags：`--server`、`--task-id`（平台侧任务 ID，缺省取 --task 目录名）、`--name-a/--token-a/--name-b/--token-b`。当 `--server` 非空时：

```go
cl := client.New(*server)
// 预检：两个 token 都有效（注册时保存）
// 每轮：
//   matchID, _, err := cl.CreateMatch(tokA, taskID, nameA, nameB)
//   上传 A：cl.UploadResult(tokA, matchID, client.ResultIn{Side: "a",
//     Passed: resA.Report.Passed, Total: resA.Report.Total, WallMS: resA.WallMS,
//     DiffHash: resA.Report.DiffHash, EventsGZ: gz(resA.Events)})
//   同理 B（tokB/side b）
//   settle 打印：第 N 轮 结算 winner=.. A分.. B分..
// 全部轮次结束后：cl.Ladder() 打印前 5 行天梯
```

事件流 gzip：`client.GzipEvents(res.Events)`。errA/errB 非 nil 的轮次：上传 `Passed: 0, Total: 0, DiffHash: ""`（技术性失败由平台按零通过处理，双零平局规则兜底），并在回调里返回 nil 继续后续轮次。
实现为独立函数 `reportRound(cl, cfg...)` 放 `mirror.go`（cmd 包内），MirrorConfig.Report 闭包调用它。

- [ ] **Step 4: 编译与冒烟**

Run: `go build ./... && go vet ./... && go test -count=1 -race ./...`
Expected: 全绿。
本地冒烟（可选，验证 CLI 参数解析）：`go run ./runner/cmd/agentbattle register --server http://127.0.0.1:1 --name x` 应报连接错误（非 flag 错误）。

- [ ] **Step 5: Commit**

```bash
git add runner/cmd/agentbattle/ runner/internal/session/mirror.go runner/internal/session/mirror_test.go
git commit -m "feat(cli): register/fetch 子命令 + mirror --server 联网上报（Elo 结算可见）"
```

---

### Task 8: 平台 E2E（server + runner client + echo agent 全链路）

**Files:**
- Create: `platform/e2e/e2e_test.go`

**注意**：`platform/e2e`（`agentbattle/platform/e2e`）**可以** import `platform/internal/api` 与 `platform/internal/store`（同子树），也可以 import `runner/internal/client`、`runner/internal/session`、`runner/internal/adapter`（跨子树 import internal 的可见性规则：Go 的 internal 限制是"被 import 方的 internal 子树外不可导入"——`agentbattle/runner/internal/...` 只允许 `agentbattle/runner/...` 前缀导入）。因此本 E2E 的 runner 侧行为通过**CLI 二进制进程**驱动（`go run`/exec）或把 E2E 放到 module 根下的 `e2e_platform_test.go`？——最简做法：**放在 `platform/e2e`，server 侧直接用 api.New + store；runner 侧用 client 包 + session 包**。client 在 `runner/internal` 下不可从 platform 导入。

**最终决定（避免 internal 边界纠纷）：把跨端 E2E 放在 module 根的 `e2e` 包外新目录 `e2e/platform_e2e_test.go`？同样受 internal 限制（module 根不在 runner/ 子树内）。**

采用方案：**`runner/internal/client` 的测试已用 stub 验证协议；跨端 E2E 通过 exec 运行 CLI 子命令进程**（`go build -o <tmp>/agentbattle.exe ./runner/cmd/agentbattle` 后 exec register/fetch/mirror），server 用 `exec.Command("go", "run", "./platform/cmd/agentbattle-server", ...)` 起在随机端口。这是真实黑盒 E2E，也顺便回归 CLI。

**Files:**
- Create: `runner/e2e/platform_e2e_test.go`（package e2e，复用 findRepoRoot）

- [ ] **Step 1: 写 E2E 测试**

```go
// runner/e2e/platform_e2e_test.go
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPlatformLoopEcho 黑盒全链路：
// 起 server → CLI 注册 A/B → fetch 任务包 → mirror --server（echo，1 轮）
// → 断言结算结果与天梯可见。
func TestPlatformLoopEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过黑盒 E2E")
	}
	root := findRepoRoot(t)

	// 构建 CLI 二进制（避免 go run 的编译输出混入 stdout 解析）
	bin := filepath.Join(t.TempDir(), "agentbattle.exe")
	out, err := exec.Command("go", "build", "-o", bin, "./runner/cmd/agentbattle").CombinedOutput()
	if err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}

	// 准备 server：独立任务目录（拷贝 examples/fix-add）
	tasksDir := t.TempDir()
	if b, err := exec.Command("cp", "-r", filepath.Join(root, "examples", "fix-add"),
		filepath.Join(tasksDir, "fix-add")).CombinedOutput(); err != nil {
		t.Fatalf("copy task: %v\n%s", err, b)
	}
	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("go", "run", "./platform/cmd/agentbattle-server",
		"--addr", addr, "--tasks", tasksDir, "--store", dbPath)
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	waitHTTP(t, "http://"+addr+"/api/ladder")

	run := func(args ...string) string {
		c := exec.Command(bin, args...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, b)
		}
		return string(b)
	}

	// 注册
	outA := run("register", "--server", "http://"+addr, "--name", "echoA")
	outB := run("register", "--server", "http://"+addr, "--name", "echoB")
	tokA := extractToken(t, outA)
	tokB := extractToken(t, outB)

	// fetch 任务包
	run("fetch", "--server", "http://"+addr, "--task", "fix-add", "--out", filepath.Join(t.TempDir(), "task"))

	// mirror --server 1 轮（echo A 修复 / B 不修复）
	rep := run("mirror", "--server", "http://"+addr, "--task", filepath.Join(t.TempDir(), "task", "fix-add"),
		"--task-id", "fix-add", "--agent", "echo", "--rounds", "1",
		"--name-a", "echoA", "--token-a", tokA, "--name-b", "echoB", "--token-b", tokB,
		"--out", filepath.Join(t.TempDir(), "reports"))
	if !strings.Contains(rep, "A 胜 1") || !strings.Contains(rep, "B 胜 0") {
		t.Fatalf("mirror output unexpected:\n%s", rep)
	}
	if !strings.Contains(rep, "1220") || !strings.Contains(rep, "1180") {
		t.Fatalf("settle ratings not visible:\n%s", rep)
	}

	// 天梯可见
	lad := run("ladder", "--server", "http://"+addr) // 若不实现 ladder 子命令则改为 http.Get 断言
	if !strings.Contains(lad, "echoA") {
		t.Fatalf("ladder missing echoA:\n%s", lad)
	}
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("server 未在期限内可用")
}

func extractToken(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "token: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "token: "))
		}
	}
	t.Fatalf("输出中无 token:\n%s", out)
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	// net.Listen(":0") 拿端口后关闭——测试环境下够用
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
```

（按需补 import `"net"`。若实现 ladder 子命令（建议：`cmd/agentbattle/ladder.go`，`--server` → 打印天梯表格，顺带补进 Task 7 的 CLI），断言其输出；否则本测试内直接 http.Get /api/ladder 解 JSON 断言——二选一，实现时定并在报告说明。`"encoding/json"` import 相应增删。）

- [ ] **Step 2: 运行确认通过**

Run: `go test ./runner/e2e/ -run TestPlatformLoopEcho -v -count=1`
Expected: PASS（server 启动 + 全链路 <60s）

- [ ] **Step 3: 全量回归**

Run: `go build ./... && go vet ./... && go test -count=1 -race ./...`
Expected: 全绿

- [ ] **Step 4: Commit**

```bash
git add runner/e2e/platform_e2e_test.go runner/cmd/agentbattle/ladder.go
git commit -m "test(e2e): 平台黑盒全链路（注册→任务包→镜像上报→Elo 结算→天梯）"
```

---

### Task 9: 真实验证（真实 claude 联网对局）+ 文档收尾

**Files:**
- Modify: `CHANGE.md`、`CLAUDE.md`（进度行）

- [ ] **Step 1: 起平台 + 注册**

```bash
go run ./platform/cmd/agentbattle-server --addr 127.0.0.1:8080 --tasks ./examples --store .scratch/platform.db &
# 另一 shell：
go run ./runner/cmd/agentbattle register --server http://127.0.0.1:8080 --name claude-real-A
go run ./runner/cmd/agentbattle register --server http://127.0.0.1:8080 --name claude-real-B
```

- [ ] **Step 2: 真实 claude 联网镜像 2 局**

```bash
# 需要 CLAUDE_CODE_GIT_BASH_PATH（反斜杠原生路径，见 CHANGE.md 遗留事项）
go run ./runner/cmd/agentbattle mirror --task ./examples/fix-add --task-id fix-add \
  --agent claude-code --rounds 2 --yolo \
  --server http://127.0.0.1:8080 \
  --name-a claude-real-A --token-a <TOK_A> \
  --name-b claude-real-B --token-b <TOK_B> \
  --out .scratch/reports
```

Expected: 每轮打印结算（winner + 双方新分），结束后打印天梯前 5。观察 2 局判分是否 2/2。

- [ ] **Step 3: 天梯核验**

```bash
go run ./runner/cmd/agentbattle ladder --server http://127.0.0.1:8080
```

Expected: claude-real-A / claude-real-B 在榜，分数偏离 1200（方向与胜负一致）。检查 `.scratch/platform.db` 所在 server 日志无异常；`SELECT COUNT(*) FROM results` 应 = 4（2 局 × 双侧）且 events_gz 非空。

- [ ] **Step 4: 文档收尾 + Commit**

CHANGE.md 追加「M1 计划 2 完成」条目（日期、交付物、真实对局数字、遗留事项——含"平台 judge_key 为固定开发密钥，按局随机下发划入后续里程碑"）。CLAUDE.md 进度行同步。

```bash
git add CHANGE.md CLAUDE.md
git commit -m "docs: M1 计划2（平台最小功能 + Runner 联网）完成记录 + 真实对局结果"
```

---

## Self-Review 记录

1. **Spec 覆盖**：注册（Task 5/7）、任务下发（Task 5 bundle + Task 7 fetch）、结果上报（Task 5/6/7）、Elo（Task 3/4）、基础天梯（Task 4/5/7）、Runner 联网对接（Task 6/7）、收官审查 3 项前置修复（Task 1/2）、E2E（Task 8）、真实验证（Task 9）。设计文档 6.1 的 K 值规则在 Task 3 固化；6.3 A/B 镜像对战即 mirror --server 的上报形态；7.2 事件 hash 链已由计划 1 的 protocol 承担，平台侧仅存储 gzip 事件流（审计流水线属 M3，不在本计划）。
2. **占位符扫描**：Task 5 的 handleCreateMatch 示意代码与 Task 6 的 FetchBundle URL 拼接已显式标注"不要照抄"的补全要求；其余步骤代码完整。
3. **类型一致性**：`ResultIn`（client）↔ API result body；`Settle{Status,Winner,RatingA,RatingB}` ↔ API 响应；`MirrorConfig.Report(ctx, round, resA, resB, errA, errB) error` 在 Task 7 定义、Task 7 CLI 消费；`judge.Run(taskDir, sandbox, baselineSHA, key)` 签名沿用计划 1 终态；`winnerOf` 与 `session/mirror.go` 的 winner 规则双向注释互指。
4. **已知取舍**：① winner 规则在 runner 与 platform 各一份（internal 边界不可共享，注释互指同步义务）；② judge_key M1 固定 dev-secret，按局随机化后置；③ SQLite 换 PostgreSQL 收敛在 store 包；④ 平台不做事件 hash 链校验（复用计划 1 已验证的 protocol.Chain，审计流水线 M3）。
