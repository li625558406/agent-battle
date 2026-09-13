# M2 计划 1：六维能力画像管道 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 平台在每场对局结算后，从已回传的事件流与判分结果确定性计算六维能力画像（0-100），并可经 API 与 CLI 查询。

**Architecture:** settle 后同步重算：`platform/internal/profile` 新包（纯函数核心：单局指标提取 → 全体池百分位归一化）+ store 扩展（task_type 列、agent_profiles 表、两个窗口查询）+ api 新路由与结算钩子 + runner CLI `profile` 子命令。无新增基础设施，覆盖式 upsert 天然幂等。

**Tech Stack:** Go 1.25（module `agentbattle`），SQLite（modernc.org/sqlite），`compress/gzip` + `encoding/json`（事件流解码），`text/tabwriter`（CLI 表格）。零新依赖。

**规格**：`docs/superpowers/specs/2026-09-13-m2-capability-profile-design.md`（归一化基线 = 同 task_type 全体 agent 近 200 局；自身窗口 = 近 50 局）

---

## 工程上下文（实现者必读）

1. **仓库布局**：Go monorepo，`platform/`（服务端：internal/store、internal/api、internal/elo、cmd/agentbattle-server）、`runner/`（CLI：cmd/agentbattle、internal/*）、`protocol/`（共享事件契约）。Go internal 边界：`platform/internal/*` 与 `runner/internal/*` 互不可导入；`protocol` 双侧可导入。
2. **gopls 误报**：本仓库 IDE gopls 大量滞后假告警（undefined/UnusedImport/UnusedVar）。一律以真实命令为准：`go build ./... && go vet ./...`。**不得**因诊断信息改动正确代码。
3. **测试命令**：单包 `go test -race ./platform/internal/store -count=1 -run TestXxx -v`；全量 `go vet ./... && go test -race ./... -count=1`（12 包；judge 的 `TestRunCommandTimeout` 存量偶发抖动，挂了单独复跑 3 次确认，不阻塞）。
4. **既有契约**：runner 侧 agent 崩溃/超时按 **0/0 上报**（`runner/cmd/agentbattle/cli_test.go:240` 固化），故 store 中 `total==0` 即崩溃局——稳定性维的判定依据。
5. **事件流**：`results.events_gz` = `[]protocol.Event` 的 NDJSON + gzip（压缩端 `runner/internal/client.GzipEvents`）。Event 字段：`Seq/TS/Type/Tool/ArgsHash/DurationMS/Tokens/Note/PrevHash/Hash`；Type ∈ tool_call/file_edit/message/error/result（`protocol/events.go`）。
6. **提交规范**：每任务一次 commit，消息用 `feat:`/`test:`/`fix:`/`docs:` 前缀 + 中文正文；提交署名 `Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>`。
7. **分支**：从 main 新建 `m2/profile`，全部提交在其上。

## 文件结构总览

| 文件 | 动作 | 职责 |
|---|---|---|
| `platform/internal/store/store.go` | 修改 | task_type 列迁移、agent_profiles 表、CreateMatch 签名、TaskTypeOf、ProfileWindow、ProfileBaseline、UpsertProfile、ProfilesByAgent |
| `platform/internal/store/store_test.go` | 修改 | 迁移/查询/幂等/upsert 测试 |
| `platform/internal/profile/metrics.go` | 新建 | 单局六维原始指标提取（纯函数） |
| `platform/internal/profile/metrics_test.go` | 新建 | 提取的对抗性测试 |
| `platform/internal/profile/profile.go` | 新建 | 百分位归一化 + 六维聚合（纯函数） |
| `platform/internal/profile/profile_test.go` | 新建 | 归一化边界测试 |
| `platform/internal/profile/recompute.go` | 新建 | gunzip 解码 + 读 store + 落库（唯一有 IO 的文件） |
| `platform/internal/profile/recompute_test.go` | 新建 | 与 store 的集成测试（幂等、task_type 隔离） |
| `platform/internal/api/api.go` | 修改 | readTaskType、CreateMatch 传参、settle 后重算钩子、GET /api/agents/{name}/profile |
| `platform/internal/api/api_test.go` | 修改 | task_type 落库、画像流转、404/空画像、重算失败不阻断 |
| `runner/internal/client/client.go` | 修改 | Profile(name) 方法与 StoredProfile 类型 |
| `runner/internal/client/client_test.go` | 修改 | Profile 响应解析测试 |
| `runner/cmd/agentbattle/profile.go` | 新建 | profile 子命令（tabwriter 表格） |
| `runner/cmd/agentbattle/profile_test.go` | 新建 | 输出格式测试 |
| `runner/cmd/agentbattle/main.go` | 修改 | 子命令分发 + usage |
| `runner/e2e/platform_e2e_test.go` | 修改 | TestProfileKnownDifference（验收：画像复现已知差异） |
| `CHANGE.md` / `CLAUDE.md` | 修改 | 迭代记录（Task 7） |

---

### Task 1: store 扩展——task_type 列、agent_profiles 表、窗口查询

**Files:**
- Modify: `platform/internal/store/store.go`
- Test: `platform/internal/store/store_test.go`

- [ ] **Step 1: 写失败测试**

在 `store_test.go` 末尾追加（该文件已有 `newTestStore` 助手与直接调用 `CreateMatch` 的既有测试——本任务改签名后既有调用点在 Step 5 一并更新）：

```go
// ---------- M2 画像：task_type 列与 agent_profiles ----------

// TestMatchTaskType 验证 task_type 经 CreateMatch 落库并可经 TaskTypeOf 回读；
// 空串落库时归一为 general。
func TestMatchTaskType(t *testing.T) {
	s := newTestStore(t)
	id1, err := s.CreateMatch("task-x", "debug", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.CreateMatch("task-y", "", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if tt, _ := s.TaskTypeOf(id1); tt != "debug" {
		t.Fatalf("task_type = %q, 期望 debug", tt)
	}
	if tt, _ := s.TaskTypeOf(id2); tt != "general" {
		t.Fatalf("空 task_type 应归一为 general, got %q", tt)
	}
	if _, err := s.TaskTypeOf(999); err == nil {
		t.Fatal("不存在的对局应报错")
	}
}

// TestProfileWindowAndBaseline 验证自身窗口（按 agent+task_type 过滤、仅
// done、按结算时间倒序）与基线（该 task_type 全体 agent）的过滤语义。
func TestProfileWindowAndBaseline(t *testing.T) {
	s := newTestStore(t)
	a1, _ := s.CreateAgent("w1")
	a2, _ := s.CreateAgent("w2")

	mk := func(taskType string, agentA, agentB int64) int64 {
		t.Helper()
		id, err := s.CreateMatch("tk", taskType, agentA, agentB)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	res := func(mid int64, side string, gz []byte) {
		t.Helper()
		if _, err := s.AddResult(mid, side, store.Result{
			Passed: 1, Total: 2, EventsGZ: gz}); err != nil {
			t.Fatal(err)
		}
	}
	// 三场 done：两场 debug（a1 与 a2 各一场自身视角），一场 general
	m1, m2, m3 := mk("debug", a1.ID, a2.ID), mk("debug", a1.ID, a2.ID), mk("general", a1.ID, a2.ID)
	res(m1, "a", nil)
	res(m1, "b", nil)
	res(m2, "a", []byte("gz2"))
	res(m2, "b", nil)
	res(m3, "a", nil)
	res(m3, "b", nil)

	own, err := s.ProfileWindow(a1.ID, "debug", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 {
		t.Fatalf("a1 debug 窗口应 2 局（general 不计入）, got %d", len(own))
	}
	if string(own[0].EventsGZ) != "gz2" {
		t.Fatalf("窗口应按结算时间倒序（m2 在前）, got %q", own[0].EventsGZ)
	}
	base, err := s.ProfileBaseline("debug", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 4 { // 两场 debug × 双侧
		t.Fatalf("debug 基线应 4 条, got %d", len(base))
	}
	if empty, _ := s.ProfileWindow(a2.ID, "nonexist", 50); len(empty) != 0 {
		t.Fatalf("无数据 task_type 窗口应为空, got %d", len(empty))
	}
}

// TestProfileUpsertAndList 验证 agent_profiles 覆盖式 upsert 与按名列举。
func TestProfileUpsertAndList(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.CreateAgent("pu")
	if err := s.UpsertProfile(a.ID, "debug", 2, `{"v":1}`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertProfile(a.ID, "debug", 3, `{"v":2}`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertProfile(a.ID, "general", 1, `{"v":3}`); err != nil {
		t.Fatal(err)
	}
	ps, err := s.ProfilesByAgent("pu")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("应 2 个 task_type 各一条, got %d", len(ps))
	}
	if ps[0].TaskType != "debug" || ps[0].SampleSize != 3 || ps[0].ProfileJSON != `{"v":2}` {
		t.Fatalf("upsert 应覆盖且按 task_type 排序: %+v", ps[0])
	}
	if ps[1].TaskType != "general" {
		t.Fatalf("第二行应为 general: %+v", ps[1])
	}
	if _, err := s.ProfilesByAgent("nobody"); err != nil {
		t.Fatalf("未知 agent 应返回空列表而非错误: %v", err)
	}
}
```

注意：上面测试用了 `store.Result`，若 `store_test.go` 在 package `store` 内则去掉 `store.` 前缀（以文件现有 package 声明为准，保持一致）。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race ./platform/internal/store -count=1 -run 'TestMatchTaskType|TestProfileWindowAndBaseline|TestProfileUpsertAndList' -v
```

预期：编译失败（`CreateMatch` 参数不符 / `TaskTypeOf`、`ProfileWindow`、`ProfileBaseline`、`UpsertProfile`、`ProfilesByAgent` undefined）。

- [ ] **Step 3: 实现 store 扩展**

`store.go` 修改点（4 处）：

(a) schema 的 `matches` 表加列（`task_type` 行插入 `status` 行之后）：

```go
CREATE TABLE IF NOT EXISTS matches (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    TEXT NOT NULL,
	task_type  TEXT NOT NULL DEFAULT 'general',
	agent_a    INTEGER NOT NULL REFERENCES agents(id),
	agent_b    INTEGER NOT NULL REFERENCES agents(id),
	status     TEXT NOT NULL DEFAULT 'pending',
	winner     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	settled_at INTEGER
);
```

schema 末尾（results 表之后）追加 agent_profiles 表：

```sql
CREATE TABLE IF NOT EXISTS agent_profiles (
	agent_id     INTEGER NOT NULL REFERENCES agents(id),
	task_type    TEXT NOT NULL DEFAULT 'general',
	sample_size  INTEGER NOT NULL DEFAULT 0,
	profile_json TEXT NOT NULL,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (agent_id, task_type)
);`
```

(b) `Open` 中 `db.Exec(schema)` 之后、`return &Store{db: db}` 之前加旧库迁移（`CREATE TABLE IF NOT EXISTS` 不会给已存在的 matches 表加列）：

```go
	// 旧库迁移：M1 时期的 matches 表没有 task_type 列。已存在时 ALTER 会报
	// "duplicate column name"，属预期，静默忽略；其余错误如实上抛。
	if _, err := db.Exec(`ALTER TABLE matches ADD COLUMN task_type TEXT NOT NULL DEFAULT 'general'`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		db.Close()
		return nil, fmt.Errorf("迁移 matches.task_type: %w", err)
	}
```

（`strings` 已在 import 列表中则不重复导入；`store.go` 现有 import 无 `strings`，需新增。）

(c) `CreateMatch` 改签名并落 task_type；其后新增 `TaskTypeOf`：

```go
func (s *Store) CreateMatch(taskID, taskType string, agentA, agentB int64) (int64, error) {
	if taskType == "" {
		taskType = "general"
	}
	res, err := s.db.Exec(
		`INSERT INTO matches (task_id, task_type, agent_a, agent_b, created_at) VALUES (?, ?, ?, ?, ?)`,
		taskID, taskType, agentA, agentB, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TaskTypeOf 返回对局的任务类型（画像按 task_type 分池）。
func (s *Store) TaskTypeOf(matchID int64) (string, error) {
	var tt string
	err := s.db.QueryRow(`SELECT task_type FROM matches WHERE id = ?`, matchID).Scan(&tt)
	return tt, err
}
```

(d) 文件末尾追加画像查询与 upsert：

```go
// ProfileMatch 是画像窗口中的一局（某 agent 视角的本方结果）。
type ProfileMatch struct {
	Passed, Total int
	WallMS        int64
	EventsGZ      []byte
}

// profileSel 是 ProfileWindow/ProfileBaseline 共用的 SELECT 体。
const profileSel = `SELECT r.passed, r.total, r.wall_ms, r.events_gz
	FROM results r JOIN matches m ON m.id = r.match_id`

func scanProfileMatches(rows *sql.Rows) ([]ProfileMatch, error) {
	defer rows.Close()
	var out []ProfileMatch
	for rows.Next() {
		var m ProfileMatch
		if err := rows.Scan(&m.Passed, &m.Total, &m.WallMS, &m.EventsGZ); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProfileWindow 自身窗口：agent 在 task_type 下最近 limit 局已完成对局的
// 本方结果，按结算时间倒序。仅 done（aborted 孤儿不参与画像）。
func (s *Store) ProfileWindow(agentID int64, taskType string, limit int) ([]ProfileMatch, error) {
	rows, err := s.db.Query(profileSel+`
		WHERE ((m.agent_a = ? AND r.side = 'a') OR (m.agent_b = ? AND r.side = 'b'))
		  AND m.task_type = ? AND m.status = 'done'
		ORDER BY m.settled_at DESC, m.id DESC LIMIT ?`,
		agentID, agentID, taskType, limit)
	if err != nil {
		return nil, err
	}
	return scanProfileMatches(rows)
}

// ProfileBaseline 归一化基线：task_type 下全体 agent 最近 limit 局已完成
// 对局的双侧结果（画像分数 = 自身局在基线分布中的百分位）。
func (s *Store) ProfileBaseline(taskType string, limit int) ([]ProfileMatch, error) {
	rows, err := s.db.Query(profileSel+`
		WHERE m.task_type = ? AND m.status = 'done'
		ORDER BY m.settled_at DESC, m.id DESC LIMIT ?`,
		taskType, limit)
	if err != nil {
		return nil, err
	}
	return scanProfileMatches(rows)
}

// StoredProfile 是 agent_profiles 的一行（ProfileJSON 由 profile 包解释）。
type StoredProfile struct {
	AgentName   string
	TaskType    string
	SampleSize  int
	ProfileJSON string
	UpdatedAt   int64
}

// UpsertProfile 覆盖式写入画像（同 agent × task_type 只保留最新）。
func (s *Store) UpsertProfile(agentID int64, taskType string, sampleSize int, profileJSON string) error {
	if taskType == "" {
		taskType = "general"
	}
	_, err := s.db.Exec(`INSERT INTO agent_profiles (agent_id, task_type, sample_size, profile_json, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, task_type) DO UPDATE SET
		  sample_size = excluded.sample_size,
		  profile_json = excluded.profile_json,
		  updated_at = excluded.updated_at`,
		agentID, taskType, sampleSize, profileJSON, time.Now().Unix())
	return err
}

// ProfilesByAgent 按名列举画像（按 task_type 升序）；未知 agent 返回空列表非错误。
func (s *Store) ProfilesByAgent(name string) ([]StoredProfile, error) {
	rows, err := s.db.Query(`SELECT a.name, ap.task_type, ap.sample_size, ap.profile_json, ap.updated_at
		FROM agent_profiles ap JOIN agents a ON a.id = ap.agent_id
		WHERE a.name = ? ORDER BY ap.task_type`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredProfile
	for rows.Next() {
		var p StoredProfile
		if err := rows.Scan(&p.AgentName, &p.TaskType, &p.SampleSize, &p.ProfileJSON, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./platform/internal/store -count=1 -v 2>&1 | tail -15
```

预期：新测试 PASS；**既有测试可能因 CreateMatch 签名编译失败**——在 Step 5 一并修复后再看全绿。

- [ ] **Step 5: 更新既有调用点并全包回归**

`grep -rn 'CreateMatch(' platform/` 找到全部调用点：生产代码 `api.go:150`（Task 5 处理）与本测试文件约 10 处。测试文件中的既有调用统一改为加 `"general"` 实参，例如：

```go
mid, err := s.CreateMatch("fix-add", "general", agA.ID, agB.ID)
```

（若某既有测试在循环里建多场，同样只加一个 `"general"`。）然后：

```bash
go build ./... && go vet ./platform/... && go test -race ./platform/internal/store -count=1
```

预期：store 包全绿（api 包暂不编译也没关系——Task 5 修；但 `go build ./...` 必须过，所以若 api.go 编译报错，此刻先在 api.go:150 临时补 `"general"` 实参，Task 5 再改为 readTaskType）。

- [ ] **Step 6: 提交**

```bash
git add platform/internal/store/ platform/internal/api/api.go
git commit -m "feat(store): task_type 列迁移 + agent_profiles 表 + 画像窗口/基线查询"
```

---

### Task 2: profile 包——单局指标提取（metrics.go）

**Files:**
- Create: `platform/internal/profile/metrics.go`
- Test: `platform/internal/profile/metrics_test.go`

- [ ] **Step 1: 写失败测试**

```go
// metrics_test.go —— 单局指标提取的对抗性测试。
package profile

import (
	"math"
	"testing"

	"agentbattle/protocol"
)

// ev 构造带 Tokens 的事件。
func ev(typ string, tokens int) protocol.Event {
	return protocol.Event{Type: typ, Tokens: tokens}
}

// TestExtractMetricsNormal 常规局：2 工具调用（1 次报错）后修复 1 编辑并全部通过。
func TestExtractMetricsNormal(t *testing.T) {
	events := []protocol.Event{
		ev(protocol.EventToolCall, 100),
		ev(protocol.EventError, 0),
		ev(protocol.EventToolCall, 200),
		ev(protocol.EventFileEdit, 0),
		ev(protocol.EventResult, 0),
	}
	m := ExtractMetrics(events, 2, 2, 900)
	if !m.HasEvents || m.ToolCalls != 2 || m.Tokens != 300 {
		t.Fatalf("基础计数错误: %+v", m)
	}
	if !m.HasErrors || m.Recovery != 1.0 {
		t.Fatalf("报错恢复应 = 通过率 1.0: %+v", m)
	}
	if !m.HasEdit || m.PreEdit != 1.0 { // 首 edit 前 tool_call=2, 总=2 → 2/2
		t.Fatalf("前置探查比应 1.0: %+v", m)
	}
	if m.ErrRatio != 0.5 { // 1 error / 2 tool_call
		t.Fatalf("无效调用率应 0.5: %+v", m)
	}
	if m.PassRatio != 1.0 || !m.AllPass || m.Crash {
		t.Fatalf("正确性字段错误: %+v", m)
	}
}

// TestExtractMetricsDegenerate 对抗性：空流/零值/崩溃局/无 edit/全 error。
func TestExtractMetricsDegenerate(t *testing.T) {
	// 空事件流 + 崩溃局（total==0）：HasEvents=false、Crash=true，除零有界
	m := ExtractMetrics(nil, 0, 0, 123)
	if m.HasEvents || !m.Crash || m.PassRatio != 0 || m.ErrRatio != 0 {
		t.Fatalf("空流崩溃局应全零且有界: %+v", m)
	}
	// 有事件但零 tool_call 的 ErrRatio 与 PreEdit 不产生 NaN
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventError, 0), ev(protocol.EventFileEdit, 0)},
		0, 3, 10)
	if math.IsNaN(m.ErrRatio) || math.IsNaN(m.PreEdit) {
		t.Fatalf("不得产生 NaN: %+v", m)
	}
	if !m.HasErrors || !m.HasEdit {
		t.Fatalf("error/edit 应被识别: %+v", m)
	}
	if m.PreEdit != 0 { // 首 edit 前 tool_call=0
		t.Fatalf("无前置调用时 PreEdit 应 0: %+v", m)
	}
	// 全 error 事件、无 tool_call：ErrRatio 有界
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventError, 5), ev(protocol.EventError, 7)},
		0, 1, 1)
	if math.IsNaN(m.ErrRatio) || m.Tokens != 12 {
		t.Fatalf("全 error 局应有界且 tokens 累加: %+v", m)
	}
	// 无 edit 局：HasEdit=false
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventToolCall, 1)}, 1, 1, 1)
	if m.HasEdit {
		t.Fatalf("无 file_edit 局 HasEdit 应为 false: %+v", m)
	}
	// 超大 tokens 累加不溢出（int64 平台内 int 足够，只验证累加正确）
	m = ExtractMetrics([]protocol.Event{ev(protocol.EventToolCall, math.MaxInt32), ev(protocol.EventToolCall, math.MaxInt32)}, 1, 1, 1)
	if m.Tokens != 2*math.MaxInt32 {
		t.Fatalf("tokens 应累加: %d", m.Tokens)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race ./platform/internal/profile -count=1 -v
```

预期：编译失败（`ExtractMetrics` undefined）。

- [ ] **Step 3: 实现 metrics.go**

```go
// metrics.go 从单局事件流与判分结果提取六维画像的原始指标（纯函数）。
// 口径见 docs/superpowers/specs/2026-09-13-m2-capability-profile-design.md §3。
package profile

import (
	"agentbattle/protocol"
)

// MatchMetrics 单局六维原始指标。布尔型参与标志（HasEvents/HasErrors/HasEdit/
// Crash）决定该局参与哪些维度的聚合——缺席局不计入对应维度样本。
type MatchMetrics struct {
	PassRatio float64 // passed/total；total==0 → 0
	AllPass   bool    // total>0 且全部通过

	HasErrors bool    // 局内含 error 事件（调试维的参与条件）
	Recovery  float64 // HasErrors 时的最终通过率（报错后恢复）

	ToolCalls int     // tool_call 事件数
	ErrRatio  float64 // error 事件数 / max(ToolCalls,1)（无效调用近似）

	HasEdit bool    // 局内含 file_edit 事件（规划维的参与条件）
	PreEdit float64 // 首个 file_edit 前的 tool_call 数 / max(ToolCalls,1)

	Tokens    int   // 事件流 tokens 累加
	WallMS    int64 // 上报的耗时
	Crash     bool  // total==0（runner 契约：崩溃/超时侧按 0/0 上报）
	HasEvents bool  // 事件流可用（工具/成本维的参与条件）
}

// ExtractMetrics 提取单局指标。events 为 nil（事件流缺失或解压失败）时
// HasEvents=false；任何除零都有界（返回 0），不产生 NaN。
func ExtractMetrics(events []protocol.Event, passed, total int, wallMS int64) MatchMetrics {
	m := MatchMetrics{WallMS: wallMS, Crash: total == 0, HasEvents: len(events) > 0}
	if total > 0 {
		m.PassRatio = float64(passed) / float64(total)
		m.AllPass = passed == total
	}
	toolCalls, errEvents := 0, 0
	for _, e := range events {
		if e.Tokens > 0 {
			m.Tokens += e.Tokens
		}
		switch e.Type {
		case protocol.EventToolCall:
			toolCalls++
		case protocol.EventError:
			errEvents++
		case protocol.EventFileEdit:
			if !m.HasEdit {
				m.PreEdit = float64(toolCalls)
				m.HasEdit = true
			}
		}
	}
	m.ToolCalls = toolCalls
	den := toolCalls
	if den == 0 {
		den = 1
	}
	m.ErrRatio = float64(errEvents) / float64(den)
	m.PreEdit = m.PreEdit / float64(den)
	if errEvents > 0 {
		m.HasErrors = true
		m.Recovery = m.PassRatio
	}
	return m
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./platform/internal/profile -count=1 -v
```

预期：PASS（2 个测试）。

- [ ] **Step 5: 提交**

```bash
git add platform/internal/profile/
git commit -m "feat(profile): 单局六维原始指标提取（空流/除零/NaN 有界）"
```

---

### Task 3: profile 包——百分位归一化与六维聚合（profile.go）

**Files:**
- Create: `platform/internal/profile/profile.go`
- Test: `platform/internal/profile/profile_test.go`

- [ ] **Step 1: 写失败测试**

```go
// profile_test.go —— 归一化与聚合的边界测试。
package profile

import (
	"testing"
)

func nearly(a, b float64) bool {
	const eps = 1e-9
	return (a-b) < eps && (b-a) < eps
}

// TestPercentileRank 基线百分位的并列中位语义。
func TestPercentileRank(t *testing.T) {
	vals := []float64{0.5, 0.5, 1, 1}
	if got := pct(vals, 1); !nearly(got, 75) { // less=2, eq=2 → (2+1)/4
		t.Fatalf("pct(1) 应 75, got %v", got)
	}
	if got := pct(vals, 0.5); !nearly(got, 25) {
		t.Fatalf("pct(0.5) 应 25, got %v", got)
	}
	if got := pct(vals, 0); got != 0 {
		t.Fatalf("pct(0) 应 0, got %v", got)
	}
	if got := pct(nil, 1); !nearly(got, 50) {
		t.Fatalf("空基线应取中位 50, got %v", got)
	}
}

// TestBuildProfileKnownDifference 验收口径的最小复现：A 全通过 vs B 半通过，
// 各 2 局（基线 = 4 局全体池）→ A 正确性 > B 正确性，且 A 高于 B 的幅度
// 与百分位公式一致。
func TestBuildProfileKnownDifference(t *testing.T) {
	good := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 4, Tokens: 100, WallMS: 100}
	bad := MatchMetrics{PassRatio: 0.5, HasEvents: true, ToolCalls: 8, Tokens: 200, WallMS: 300}
	ownA := []MatchMetrics{good, good}
	ownB := []MatchMetrics{bad, bad}
	baseline := []MatchMetrics{good, good, bad, bad}

	pa := BuildProfile("A", "debug", ownA, baseline, 1000)
	pb := BuildProfile("B", "debug", ownB, baseline, 1000)

	if pa.SampleSize != 2 || !pa.LowSample { // 2 < 5
		t.Fatalf("样本量与 low_sample 标注错误: %+v", pa)
	}
	if pa.Dims["correctness"].Score <= pb.Dims["correctness"].Score {
		t.Fatalf("已知差异未复现: A=%v B=%v", pa.Dims["correctness"], pb.Dims["correctness"])
	}
	// 精确值：A 的 PassRatio=1 在 [1,1,.5,.5] 中 pct=75，AllPass 同 → correctness=75
	if !nearly(pa.Dims["correctness"].Score, 75) {
		t.Fatalf("A correctness 应 75, got %v", pa.Dims["correctness"].Score)
	}
	if !nearly(pb.Dims["correctness"].Score, 25) {
		t.Fatalf("B correctness 应 25, got %v", pb.Dims["correctness"].Score)
	}
	// 成本维（tokens/wall 低好）：A 两项都在基线低位 → 高分
	if pa.Dims["cost"].Score <= pb.Dims["cost"].Score {
		t.Fatalf("成本维方向错误: A=%v B=%v", pa.Dims["cost"], pb.Dims["cost"])
	}
}

// TestBuildProfileEdge 缺席规则与空窗口：无错误局调试维满分、无 edit 局
// 规划维零样本、空 own 窗口返回零值画像。
func TestBuildProfileEdge(t *testing.T) {
	noErr := MatchMetrics{PassRatio: 1, AllPass: true, HasEvents: true, ToolCalls: 2, Tokens: 10, WallMS: 10}
	p := BuildProfile("A", "t", []MatchMetrics{noErr, noErr}, []MatchMetrics{noErr, noErr}, 1)
	if d := p.Dims["debugging"]; d.Score != 100 || d.Raw != 1 {
		t.Fatalf("全无错误局调试维应满分: %+v", d)
	}
	if d := p.Dims["planning"]; d.Sample != 0 || d.Score != 0 {
		t.Fatalf("无 edit 局规划维应零样本零分: %+v", d)
	}

	empty := BuildProfile("A", "t", nil, nil, 1)
	if empty.SampleSize != 0 || empty.Dims != nil {
		t.Fatalf("空窗口应返回零值画像: %+v", empty)
	}

	// 崩溃局参与稳定性维：Crash 局在基线中低好 → 崩溃多的 agent 稳定性低分
	crash := MatchMetrics{Crash: true}
	pBad := BuildProfile("C", "t", []MatchMetrics{crash, crash}, []MatchMetrics{noErr, noErr, crash, crash}, 1)
	pGood := BuildProfile("D", "t", []MatchMetrics{noErr, noErr}, []MatchMetrics{noErr, noErr, crash, crash}, 1)
	if pBad.Dims["stability"].Score >= pGood.Dims["stability"].Score {
		t.Fatalf("崩溃 agent 稳定性应更低: bad=%v good=%v",
			pBad.Dims["stability"].Score, pGood.Dims["stability"].Score)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race ./platform/internal/profile -count=1 -run 'TestPercentileRank|TestBuildProfile' -v
```

预期：编译失败（`pct`、`BuildProfile` undefined）。

- [ ] **Step 3: 实现 profile.go**

```go
// profile.go 六维聚合与百分位归一化（纯函数）。
// 归一化基线 = 同 task_type 全体 agent 的近期对局池（调用方经 store 的
// ProfileBaseline 取得）；自身窗口决定参与局与样本量。
package profile

// 六维名称（Profile.Dims 的键）。
const (
	DimCorrect = "correctness"
	DimDebug   = "debugging"
	DimTool    = "tool_efficiency"
	DimCost    = "cost"
	DimPlan    = "planning"
	DimStab    = "stability"
)

// lowSampleThreshold 样本低于该值时画像标注 low_sample。
const lowSampleThreshold = 5

// Dim 单维：Score=归一化分（0-100），Raw=自身窗口主指标均值，Sample=参与局数。
type Dim struct {
	Score  float64 `json:"score"`
	Raw    float64 `json:"raw"`
	Sample int     `json:"sample"`
}

// Profile 完整画像。
type Profile struct {
	Agent      string         `json:"agent"`
	TaskType   string         `json:"task_type"`
	SampleSize int            `json:"sample_size"`
	LowSample  bool           `json:"low_sample"`
	Dims       map[string]Dim `json:"dims"`
	UpdatedAt  int64          `json:"updated_at"`
}

// pct 返回 v 在基线 vals 中的百分位（0-100）：
// (小于 v 的个数 + 0.5*等于 v 的个数)/n*100 —— 并列取中间档。
// 基线为空（新任务类型首批对局）时无信息可排名，一律取中位 50。
func pct(vals []float64, v float64) float64 {
	if len(vals) == 0 {
		return 50
	}
	less, eq := 0, 0
	for _, x := range vals {
		switch {
		case x < v:
			less++
		case x == v:
			eq++
		}
	}
	return (float64(less) + 0.5*float64(eq)) / float64(len(vals)) * 100
}

// collect 取基线中参与某指标的值集合。
func collect(ms []MatchMetrics, val func(MatchMetrics) (float64, bool)) []float64 {
	out := make([]float64, 0, len(ms))
	for _, m := range ms {
		if v, ok := val(m); ok {
			out = append(out, v)
		}
	}
	return out
}

// oneDim 计算单个指标维：own 中可参与的局逐个在基线取百分位（低好取反）
// 后求均值。无可参与局时 ok=false。
func oneDim(own, baseVals []float64, lowGood bool) (Dim, bool) {
	if len(own) == 0 {
		return Dim{}, false
	}
	var sum float64
	for _, v := range own {
		p := pct(baseVals, v)
		if lowGood {
			p = 100 - p
		}
		sum += p
	}
	return Dim{Score: sum / float64(len(own)), Sample: len(own)}, true
}

// mergeDim 合成一维的两指标分：只平均有样本的指标；都无样本返回零值。
func mergeDim(a, b Dim, raw float64) Dim {
	switch {
	case a.Sample > 0 && b.Sample > 0:
		return Dim{Score: (a.Score + b.Score) / 2, Raw: raw, Sample: a.Sample}
	case a.Sample > 0:
		return Dim{Score: a.Score, Raw: raw, Sample: a.Sample}
	case b.Sample > 0:
		return Dim{Score: b.Score, Raw: raw, Sample: b.Sample}
	}
	return Dim{}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// BuildProfile 聚合自身窗口 own 与全体基线 baseline，产出完整画像。
// own 为空时返回零值画像（Dims=nil，查询侧可区分"无对局"）。
func BuildProfile(agent, taskType string, own, baseline []MatchMetrics, now int64) Profile {
	p := Profile{Agent: agent, TaskType: taskType, SampleSize: len(own),
		LowSample: len(own) > 0 && len(own) < lowSampleThreshold, UpdatedAt: now}
	if len(own) == 0 {
		return p
	}
	p.Dims = map[string]Dim{}

	// 正确性：PassRatio + AllPass（均高好）
	passBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.PassRatio, true })
	allBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return b2f(m.AllPass), true })
	ownPass := collect(own, func(m MatchMetrics) (float64, bool) { return m.PassRatio, true })
	ownAll := collect(own, func(m MatchMetrics) (float64, bool) { return b2f(m.AllPass), true })
	d1, _ := oneDim(ownPass, passBase, false)
	d2, _ := oneDim(ownAll, allBase, false)
	rawPass := 0.0
	for _, m := range own {
		rawPass += m.PassRatio
	}
	p.Dims[DimCorrect] = mergeDim(d1, d2, rawPass/float64(len(own)))

	// 调试：Recovery（高好），仅 HasErrors 局；全窗无错误局 → 满分（无试错
	// 即无失败，视为该维无负担）
	hasErr := 0
	sumRec := 0.0
	for _, m := range own {
		if m.HasErrors {
			hasErr++
			sumRec += m.Recovery
		}
	}
	if hasErr == 0 {
		p.Dims[DimDebug] = Dim{Score: 100, Raw: 1, Sample: len(own)}
	} else {
		recBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.Recovery, m.HasErrors })
		ownRec := collect(own, func(m MatchMetrics) (float64, bool) { return m.Recovery, m.HasErrors })
		d, _ := oneDim(ownRec, recBase, false)
		p.Dims[DimDebug] = Dim{Score: d.Score, Raw: sumRec / float64(hasErr), Sample: hasErr}
	}

	// 工具效率：ToolCalls + ErrRatio（均低好），仅 HasEvents 局
	tcBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return float64(m.ToolCalls), m.HasEvents })
	erBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.ErrRatio, m.HasEvents })
	ownTC := collect(own, func(m MatchMetrics) (float64, bool) { return float64(m.ToolCalls), m.HasEvents })
	ownER := collect(own, func(m MatchMetrics) (float64, bool) { return m.ErrRatio, m.HasEvents })
	d1, _ = oneDim(ownTC, tcBase, true)
	d2, _ = oneDim(ownER, erBase, true)
	rawTC := 0.0
	for _, m := range own {
		if m.HasEvents {
			rawTC += float64(m.ToolCalls)
		}
	}
	p.Dims[DimTool] = mergeDim(d1, d2, rawTC/float64(max(1, countEvents(own))))

	// 成本：Tokens + WallMS（均低好），仅 HasEvents 局（无事件流时 tokens=0
	// 会假性最优，必须排除）
	tkBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return float64(m.Tokens), m.HasEvents })
	wlBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return float64(m.WallMS), m.HasEvents })
	ownTK := collect(own, func(m MatchMetrics) (float64, bool) { return float64(m.Tokens), m.HasEvents })
	ownWL := collect(own, func(m MatchMetrics) (float64, bool) { return float64(m.WallMS), m.HasEvents })
	d1, _ = oneDim(ownTK, tkBase, true)
	d2, _ = oneDim(ownWL, wlBase, true)
	p.Dims[DimCost] = mergeDim(d1, d2, 0)

	// 规划：PreEdit（高好），仅 HasEdit 局
	peBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return m.PreEdit, m.HasEdit })
	ownPE := collect(own, func(m MatchMetrics) (float64, bool) { return m.PreEdit, m.HasEdit })
	if d, ok := oneDim(ownPE, peBase, false); ok {
		rawPE := 0.0
		n := 0
		for _, m := range own {
			if m.HasEdit {
				rawPE += m.PreEdit
				n++
			}
		}
		p.Dims[DimPlan] = Dim{Score: d.Score, Raw: rawPE / float64(n), Sample: n}
	} else {
		p.Dims[DimPlan] = Dim{}
	}

	// 稳定性：Crash 0/1 + 通过率偏离窗口均值（均低好）
	crashBase := collect(baseline, func(m MatchMetrics) (float64, bool) { return b2f(m.Crash), true })
	ownCrash := collect(own, func(m MatchMetrics) (float64, bool) { return b2f(m.Crash), true })
	mean := 0.0
	nTotal := 0
	for _, m := range own {
		if !m.Crash {
			mean += m.PassRatio
			nTotal++
		}
	}
	if nTotal > 0 {
		mean /= float64(nTotal)
	}
	devBase := collect(baseline, func(m MatchMetrics) (float64, bool) {
		return dev(m, mean), !m.Crash
	})
	ownDev := collect(own, func(m MatchMetrics) (float64, bool) {
		return dev(m, mean), !m.Crash
	})
	d1, _ = oneDim(ownCrash, crashBase, true)
	d2, _ = oneDim(ownDev, devBase, true)
	crashN := 0
	for _, m := range own {
		if m.Crash {
			crashN++
		}
	}
	p.Dims[DimStab] = mergeDim(d1, d2, float64(crashN)/float64(len(own)))
	return p
}

// dev 局通过率对参考均值的偏离（稳定性维的第二指标；崩溃局不参与）。
func dev(m MatchMetrics, mean float64) float64 {
	d := m.PassRatio - mean
	if d < 0 {
		return -d
	}
	return d
}

func countEvents(ms []MatchMetrics) int {
	n := 0
	for _, m := range ms {
		if m.HasEvents {
			n++
		}
	}
	return n
}
```

（Go 1.21+ 内置 `max` 可直接用。）

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./platform/internal/profile -count=1 -v
```

预期：4 个测试全 PASS。

- [ ] **Step 5: 提交**

```bash
git add platform/internal/profile/
git commit -m "feat(profile): 全体池百分位归一化与六维聚合（缺席规则有定义）"
```

---

### Task 4: profile 包——重算管道（recompute.go）

**Files:**
- Create: `platform/internal/profile/recompute.go`
- Test: `platform/internal/profile/recompute_test.go`

- [ ] **Step 1: 写失败测试**

```go
// recompute_test.go —— 与 store 的集成测试。
package profile

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"path/filepath"
	"testing"

	"agentbattle/protocol"
	"agentbattle/platform/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// gzEvents 把事件流压成 events_gz 形态（与 runner 侧 GzipEvents 同构）。
func gzEvents(t *testing.T, events []protocol.Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestRecomputeIdempotentAndIsolated 跑通"读窗口 → 聚合 → 落库"，两次重算
// 的维度分完全一致（幂等），且 task_type 互不污染。
func TestRecomputeIdempotentAndIsolated(t *testing.T) {
	s := newStore(t)
	a, _ := s.CreateAgent("ra")
	b, _ := s.CreateAgent("rb")
	events := []protocol.Event{{Type: protocol.EventToolCall, Tokens: 10}, {Type: protocol.EventFileEdit}}

	mk := func(taskType string, passed int) int64 {
		t.Helper()
		mid, err := s.CreateMatch("tk", taskType, a.ID, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		for side, p := range map[string]int{"a": passed, "b": 0} {
			gz := gzEvents(t, events)
			if p == 0 && side == "b" {
				gz = nil // B 侧事件流缺失：降级路径
			}
			if _, err := s.AddResult(mid, side, store.Result{Passed: p, Total: 2, EventsGZ: gz}); err != nil {
				t.Fatal(err)
			}
		}
		return mid
	}
	mk("debug", 2)
	mk("debug", 2)
	mk("general", 2)

	if err := Recompute(s, a.ID, "debug"); err != nil {
		t.Fatal(err)
	}
	ps1, err := s.ProfilesByAgent("ra")
	if err != nil || len(ps1) != 1 {
		t.Fatalf("debug 池重算后应 1 条画像: %v %v", ps1, err)
	}
	first := ps1[0].ProfileJSON

	if err := Recompute(s, a.ID, "debug"); err != nil {
		t.Fatal(err)
	}
	ps2, _ := s.ProfilesByAgent("ra")
	if ps2[0].ProfileJSON != first {
		t.Fatalf("同数据两次重算应幂等（维度分一致）:\n%s\n%s", first, ps2[0].ProfileJSON)
	}

	// general 是独立池：重算后 debug 画像不受影响
	if err := Recompute(s, a.ID, "general"); err != nil {
		t.Fatal(err)
	}
	ps3, _ := s.ProfilesByAgent("ra")
	if len(ps3) != 2 {
		t.Fatalf("task_type 间应互相隔离: %d", len(ps3))
	}
	for _, p := range ps3 {
		if p.TaskType == "debug" && p.ProfileJSON != first {
			t.Fatal("debug 画像不应被 general 重算波及")
		}
	}
	// 画像可反序列化回 Profile 且样本量正确
	for _, p := range ps3 {
		var doc Profile
		if err := json.Unmarshal([]byte(p.ProfileJSON), &doc); err != nil {
			t.Fatalf("profile_json 应为合法 Profile JSON: %v", err)
		}
		if doc.SampleSize != 2 || len(doc.Dims) != 6 {
			t.Fatalf("样本与维度数错误: %+v", doc)
		}
	}
}

// TestRecomputeNoMatches 无任何对局时不落画像（空窗口不产生垃圾行）。
func TestRecomputeNoMatches(t *testing.T) {
	s := newStore(t)
	a, _ := s.CreateAgent("empty")
	if err := Recompute(s, a.ID, "general"); err != nil {
		t.Fatalf("空窗口重算不应报错: %v", err)
	}
	ps, _ := s.ProfilesByAgent("empty")
	if len(ps) != 0 {
		t.Fatalf("空窗口不应落库: %d", len(ps))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race ./platform/internal/profile -count=1 -run TestRecompute -v
```

预期：编译失败（`Recompute` undefined）。

- [ ] **Step 3: 实现 recompute.go**

```go
// recompute.go 画像重算管道：读 store 窗口与基线 → 聚合 → 覆盖落库。
// 本文件是 profile 包唯一有 IO 的部分；幂等：同数据重算结果一致（UpdatedAt
// 除外），重算失败保留旧画像。
package profile

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"time"

	"agentbattle/platform/internal/store"
	"agentbattle/protocol"
)

// 窗口与基线规模（与规格 §3 一致）。
const (
	windowSize   = 50
	baselineSize = 200
)

// Recompute 重建 agent 在 taskType 下的画像并落库。自身窗口为空时不落库
//（避免无对局也产生画像行）。事件流解压失败的局按 HasEvents=false 降级。
func Recompute(st *store.Store, agentID int64, taskType string) error {
	ag, err := st.AgentByID(agentID)
	if err != nil {
		return fmt.Errorf("读 agent %d: %w", agentID, err)
	}
	ownRows, err := st.ProfileWindow(agentID, taskType, windowSize)
	if err != nil {
		return fmt.Errorf("读窗口: %w", err)
	}
	if len(ownRows) == 0 {
		return nil
	}
	baseRows, err := st.ProfileBaseline(taskType, baselineSize)
	if err != nil {
		return fmt.Errorf("读基线: %w", err)
	}
	own := make([]MatchMetrics, len(ownRows))
	for i, r := range ownRows {
		own[i] = ExtractMetrics(decodeEvents(r.EventsGZ), r.Passed, r.Total, r.WallMS)
	}
	base := make([]MatchMetrics, len(baseRows))
	for i, r := range baseRows {
		base[i] = ExtractMetrics(decodeEvents(r.EventsGZ), r.Passed, r.Total, r.WallMS)
	}
	p := BuildProfile(ag.Name, taskType, own, base, time.Now().Unix())
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("序列化画像: %w", err)
	}
	return st.UpsertProfile(agentID, taskType, p.SampleSize, string(b))
}

// decodeEvents 解压 NDJSON 事件流；任何失败返回 nil（该局降级为无事件流，
// 由 ExtractMetrics/BuildProfile 的缺席规则处理，不阻断整场画像）。
func decodeEvents(gz []byte) []protocol.Event {
	if len(gz) == 0 {
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil
	}
	defer zr.Close()
	var events []protocol.Event
	dec := json.NewDecoder(zr)
	for {
		var e protocol.Event
		if err := dec.Decode(&e); err != nil {
			break
		}
		events = append(events, e)
	}
	return events
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./platform/internal/profile -count=1 -v
```

预期：6 个测试全 PASS。

- [ ] **Step 5: 提交**

```bash
git add platform/internal/profile/
git commit -m "feat(profile): 重算管道——读窗口/基线 → 聚合 → 覆盖落库（降级有界）"
```

---

### Task 5: api——task_type 落库、结算钩子、画像查询路由

**Files:**
- Modify: `platform/internal/api/api.go`
- Test: `platform/internal/api/api_test.go`

- [ ] **Step 1: 写失败测试**

`api_test.go` 末尾追加（复用既有 `newServerWithStore`/`register`/`do` 助手；`demo` 任务目录的 task.json 无 task_type 字段 → 落库应归一 general）：

```go
// ---------- M2 画像：task_type / 结算钩子 / 查询路由 ----------

// profileResp 是 GET /api/agents/{name}/profile 的响应形态。
type profileResp struct {
	Agent    string `json:"agent"`
	Profiles []struct {
		TaskType    string `json:"task_type"`
		SampleSize  int    `json:"sample_size"`
		ProfileJSON string `json:"profile_json"`
		UpdatedAt   int64  `json:"updated_at"`
	} `json:"profiles"`
}

// playMatch 经 HTTP 注册两名 agent、创建对局、双侧上报（A 全过 / B 半过）
// 触发结算，返回双方 token。
func playMatch(t *testing.T, srv *httptest.Server, nameA, nameB string) (tokA, tokB string) {
	t.Helper()
	tokA = register(t, srv, nameA)
	tokB = register(t, srv, nameB)
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": nameA, "agent_b": nameB})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局: %d", resp.StatusCode)
	}
	mid := int64(m["match_id"].(float64))
	up := func(tok string, side string, passed int) {
		t.Helper()
		body := map[string]any{"side": side, "passed": passed, "total": 2, "wall_ms": 100}
		if side == "a" {
			body["events_gz_base64"] = base64.StdEncoding.EncodeToString([]byte{})
		}
		resp, _ := do(t, "POST", fmt.Sprintf("%s/api/matches/%d/results", srv.URL, mid), tok, body)
		if resp.StatusCode != 200 {
			t.Fatalf("上报 %s: %d", side, resp.StatusCode)
		}
	}
	up(tokA, "a", 2)
	up(tokB, "b", 1)
	return tokA, tokB
}

// TestProfileFlow 结算后画像落库、路由可查、404/空画像三分支。
func TestProfileFlow(t *testing.T) {
	srv, _ := newServerWithStore(t)
	playMatch(t, srv, "pa", "pb")

	resp, m := do(t, "GET", srv.URL+"/api/agents/pa/profile", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("查画像: %d", resp.StatusCode)
	}
	b, _ := json.Marshal(m)
	var pr profileResp
	if err := json.Unmarshal(b, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Agent != "pa" || len(pr.Profiles) != 1 {
		t.Fatalf("应 1 条画像: %+v", pr)
	}
	prof := pr.Profiles[0]
	if prof.TaskType != "general" { // demo task.json 无 task_type → 归一
		t.Fatalf("task_type 应归一 general: %+v", prof)
	}
	if prof.SampleSize != 1 || !strings.Contains(prof.ProfileJSON, `"correctness"`) {
		t.Fatalf("画像内容不符: %+v", prof)
	}

	// 未知 agent → 404；已知 agent 无对局 → 200 空数组
	if resp, _ = do(t, "GET", srv.URL+"/api/agents/ghost/profile", "", nil); resp.StatusCode != 404 {
		t.Fatalf("未知 agent 应 404, got %d", resp.StatusCode)
	}
	if _, err := do(t, "GET", srv.URL+"/api/agents/pa/profile", "", nil); err != nil {
		t.Fatal(err)
	}
	tokC := register(t, srv, "pc")
	_ = tokC
	resp, _ = do(t, "GET", srv.URL+"/api/agents/pc/profile", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("无对局 agent 应 200, got %d", resp.StatusCode)
	}
}

// TestProfileKnownDifferenceAPI A 全过 vs B 半过 → A 的 correctness 分严格
// 高于 B（归一化基线全体池语义在 API 层的验收）。
func TestProfileKnownDifferenceAPI(t *testing.T) {
	srv, _ := newServerWithStore(t)
	playMatch(t, srv, "ka", "kb")

	get := func(name string) float64 {
		t.Helper()
		_, m := do(t, "GET", srv.URL+"/api/agents/"+name+"/profile", "", nil)
		b, _ := json.Marshal(m)
		var pr profileResp
		if err := json.Unmarshal(b, &pr); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Dims map[string]struct {
				Score float64 `json:"score"`
			} `json:"dims"`
		}
		if err := json.Unmarshal([]byte(pr.Profiles[0].ProfileJSON), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Dims["correctness"].Score
	}
	sa, sb := get("ka"), get("kb")
	if sa <= sb {
		t.Fatalf("画像应复现已知差异: A=%v B=%v", sa, sb)
	}
}
```

（`api_test.go` 需补 import `"encoding/base64"` 与 `"fmt"`——检查既有 import，缺则加。）

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race ./platform/internal/api -count=1 -run 'TestProfile' -v
```

预期：`TestProfileFlow` 挂——GET profile 路由 404（路由未注册），结算后画像未落库。

- [ ] **Step 3: 实现 api.go 三处修改**

(a) import 增 `"agentbattle/platform/internal/profile"`。

(b) `New` 的 mux 装配中加一行（ladder 之后）：

```go
	mux.HandleFunc("GET /api/agents/{name}/profile", s.handleProfile)
```

(c) `handleCreateMatch` 的 `s.St.CreateMatch(body.TaskID, a.ID, b.ID)` 改为：

```go
	id, err := s.St.CreateMatch(body.TaskID, readTaskType(s.TasksDir, body.TaskID), a.ID, b.ID)
```

(d) `handleResult` 中"已结算：回读 winner"之前（`if !done { ... return }` 之后）插入重算钩子：

```go
	// 画像重算（M2）：结算成功后同步重建双方画像。失败仅记日志降级，
	// 不阻断结算响应——幂等保证下局结算重算自愈。
	if tt, terr := s.St.TaskTypeOf(matchID); terr != nil {
		log.Printf("对局 %d 读 task_type 失败（跳过画像重算）: %v", matchID, terr)
	} else {
		for _, aid := range []int64{aID, bID} {
			if rerr := profile.Recompute(s.St, aid, tt); rerr != nil {
				log.Printf("对局 %d 画像重算失败（agent %d）: %v", matchID, aid, rerr)
			}
		}
	}
```

(e) 文件末尾新增两个 handler：

```go
// readTaskType 从任务目录 task.json 读取 task_type。缺字段/解析失败/空值
// 一律归一 "general"（旧任务包向后兼容；任务元数据不可信，宁缺毋滥）。
func readTaskType(tasksDir, taskID string) string {
	b, err := os.ReadFile(filepath.Join(tasksDir, taskID, "task.json"))
	if err != nil {
		return "general"
	}
	var meta struct {
		TaskType string `json:"task_type"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return "general"
	}
	if tt := strings.TrimSpace(meta.TaskType); tt != "" {
		return tt
	}
	return "general"
}

// handleProfile GET /api/agents/{name}/profile：agent 全部 task_type 的画像。
// 未知 agent → 404；已知 agent 无对局 → 200 空数组（语义区分）。
// 公开路由（同天梯）：画像只含 agent 名与分数，无 token 无配置内容。
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok, err := s.St.AgentByName(name); err != nil || !ok {
		writeErr(w, http.StatusNotFound, "agent 不存在")
		return
	}
	profs, err := s.St.ProfilesByAgent(name)
	if err != nil {
		log.Printf("读 agent %q 画像失败: %v", name, err)
		writeErr(w, http.StatusInternalServerError, "读画像失败")
		return
	}
	if profs == nil {
		profs = []store.StoredProfile{}
	}
	type profileOut struct {
		TaskType    string `json:"task_type"`
		SampleSize  int    `json:"sample_size"`
		ProfileJSON string `json:"profile_json"`
		UpdatedAt   int64  `json:"updated_at"`
	}
	out := make([]profileOut, len(profs))
	for i, p := range profs {
		out[i] = profileOut{TaskType: p.TaskType, SampleSize: p.SampleSize,
			ProfileJSON: p.ProfileJSON, UpdatedAt: p.UpdatedAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "profiles": out})
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./platform/internal/api -count=1 -v 2>&1 | tail -12
```

预期：全 PASS（含既有测试——`TestResultRejectedAfterSweep` 等不受影响）。

- [ ] **Step 5: 提交**

```bash
git add platform/internal/api/
git commit -m "feat(api): task_type 落库 + settle 后画像重算钩子 + GET profile 路由"
```

---

### Task 6: client.Profile + CLI profile 子命令

**Files:**
- Modify: `runner/internal/client/client.go`、`runner/internal/client/client_test.go`
- Create: `runner/cmd/agentbattle/profile.go`、`runner/cmd/agentbattle/profile_test.go`
- Modify: `runner/cmd/agentbattle/main.go`

- [ ] **Step 1: 写失败测试**

`client_test.go` 末尾追加（沿用该文件既有的 httptest 模式）：

```go
// TestProfile 解析 /api/agents/{name}/profile 响应；路径转义防注入。
func TestProfile(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agent":"a/b","profiles":[` +
			`{"task_type":"general","sample_size":2,` +
			`"profile_json":"{\"dims\":{\"correctness\":{\"score\":75}}}",` +
			`"updated_at":123}]}`)
	}))
	defer srv.Close()
	profs, err := client.New(srv.URL).Profile("a/b")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/agents/a/b/profile" { // url.PathEscape 只转义 query 语义，Path 段内 / 保留
		t.Fatalf("路径错误: %s", gotPath)
	}
	if len(profs) != 1 || profs[0].TaskType != "general" || profs[0].SampleSize != 2 {
		t.Fatalf("解析错误: %+v", profs)
	}
	if !strings.Contains(profs[0].ProfileJSON, "correctness") {
		t.Fatalf("profile_json 应原样透传: %q", profs[0].ProfileJSON)
	}
}
```

（以 `client_test.go` 现有 import 为准补 `"strings"` 等。）

`profile_test.go`（新文件）：

```go
// profile_test.go —— CLI profile 子命令的输出格式测试。
package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrintProfile 六维固定顺序输出 + 低样本提示；空画像打印引导文案。
func TestPrintProfile(t *testing.T) {
	var buf bytes.Buffer
	doc := profileDoc{
		SampleSize: 2,
		LowSample:  true,
		Dims: map[string]profileDim{
			"correctness": {Score: 75, Raw: 1, Sample: 2},
			"stability":   {Score: 62.5, Raw: 0, Sample: 2},
		},
	}
	printProfile(&buf, "demoA", "general", doc)
	out := buf.String()
	for _, want := range []string{"demoA", "general", "样本不足", "correctness", "75.0", "stability", "62.5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺 %q:\n%s", want, out)
		}
	}
	// 六维顺序：correctness 在 tool_efficiency 前
	if strings.Index(out, "correctness") > strings.Index(out, "tool_efficiency") {
		t.Fatalf("维度顺序错误:\n%s", out)
	}

	buf.Reset()
	printProfile(&buf, "nobody", "general", profileDoc{})
	if !strings.Contains(buf.String(), "暂无") {
		t.Fatalf("零样本应打印提示:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race ./runner/internal/client -run TestProfile -count=1 -v ; go test -race ./runner/cmd/agentbattle -run TestPrintProfile -count=1 -v
```

预期：编译失败（`Profile` 方法、`profileDoc`、`printProfile` undefined）。

- [ ] **Step 3: 实现**

`client.go` 末尾追加：

```go
// StoredProfile 是平台返回的一条画像（ProfileJSON 为平台侧 profile 包
// 序列化的 JSON，runner 不解释其内部结构——由 CLI 层按需解析）。
type StoredProfile struct {
	TaskType    string `json:"task_type"`
	SampleSize  int    `json:"sample_size"`
	ProfileJSON string `json:"profile_json"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Profile 拉取指定 agent 的全部画像（公开路由，无需 token）。
func (c *Client) Profile(name string) ([]StoredProfile, error) {
	var out struct {
		Agent    string          `json:"agent"`
		Profiles []StoredProfile `json:"profiles"`
	}
	path := "/api/agents/" + url.PathEscape(name) + "/profile"
	if err := c.do(http.MethodGet, path, "", nil, &out); err != nil {
		return nil, err
	}
	return out.Profiles, nil
}
```

`runner/cmd/agentbattle/profile.go`（新文件）：

```go
// profile.go 实现 agentbattle profile 子命令：查询并打印六维能力画像。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"agentbattle/runner/internal/client"
)

// profileDim/profileDoc 是 CLI 侧的画像 JSON 视图（runner 不能导入
// platform/internal/profile——internal 边界，故本地定义最小解析形态）。
type profileDim struct {
	Score  float64 `json:"score"`
	Raw    float64 `json:"raw"`
	Sample int     `json:"sample"`
}

type profileDoc struct {
	SampleSize int                   `json:"sample_size"`
	LowSample  bool                  `json:"low_sample"`
	Dims       map[string]profileDim `json:"dims"`
}

// dimOrder 六维固定展示顺序。
var dimOrder = []string{"correctness", "debugging", "tool_efficiency", "cost", "planning", "stability"}

// cmdProfile 解析参数并打印画像。
func cmdProfile(args []string) error {
	fs := newFlagSet("profile")
	server := fs.String("server", "", "平台 API 根地址（必填）")
	name := fs.String("name", "", "agent 名（必填）")
	if helped, err := parseFlags(fs, args); err != nil || helped {
		return err
	}
	if *server == "" {
		return fmt.Errorf("--server 必填")
	}
	if *name == "" {
		return fmt.Errorf("--name 必填")
	}
	profs, err := client.New(*server).Profile(*name)
	if err != nil {
		return err
	}
	for i, p := range profs {
		var doc profileDoc
		if err := json.Unmarshal([]byte(p.ProfileJSON), &doc); err != nil {
			return fmt.Errorf("画像 %d JSON 解析失败: %w", i, err)
		}
		if i > 0 {
			fmt.Fprintln(os.Stdout)
		}
		printProfile(os.Stdout, *name, p.TaskType, doc)
	}
	if len(profs) == 0 {
		fmt.Fprintf(os.Stdout, "%s 暂无画像：完成对局后自动生成\n", *name)
	}
	return nil
}

// printProfile 以 tabwriter 输出单个 task_type 的六维表。
func printProfile(w io.Writer, agent, taskType string, doc profileDoc) {
	fmt.Fprintf(w, "agent %s · 任务类型 %s · 样本 %d 局", agent, taskType, doc.SampleSize)
	if doc.LowSample {
		fmt.Fprint(w, "（样本不足，分数仅供参考）")
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "维度\t得分\t原始值\t样本")
	for _, dim := range dimOrder {
		d, ok := doc.Dims[dim]
		if !ok {
			fmt.Fprintf(tw, "%s\t—\t—\t0\n", dim)
			continue
		}
		fmt.Fprintf(tw, "%s\t%.1f\t%.2f\t%d\n", dim, d.Score, d.Raw, d.Sample)
	}
	tw.Flush()
}
```

`main.go` 两处：usage 文本 `agentbattle ladder   --server URL` 行后加：

```
  agentbattle profile  --server URL --name X
```

分发 switch 的 `case "ladder"` 块后加：

```go
	case "profile":
		err = cmdProfile(os.Args[2:])
```

文件头 doc 注释的子命令列表补 `profile（查看能力画像）`。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./runner/internal/client ./runner/cmd/agentbattle -count=1 2>&1 | tail -4
```

预期：两包全绿。

- [ ] **Step 5: 提交**

```bash
git add runner/internal/client/ runner/cmd/agentbattle/
git commit -m "feat(cli): profile 子命令——查询并展示六维能力画像"
```

---

### Task 7: E2E 验收 + 全量回归 + 文档

**Files:**
- Modify: `runner/e2e/platform_e2e_test.go`
- Modify: `CHANGE.md`、`CLAUDE.md`

- [ ] **Step 1: 写失败测试（黑盒 E2E）**

`platform_e2e_test.go` 末尾追加（复用文件内既有 `build` 所用模式——但 `build` 闭包在 `TestPlatformLoopEcho` 内部；抽出逻辑等价的本地实现，或把既有 build/waitHTTP 提为包级测试助手。**选后者**：把 `TestPlatformLoopEcho` 内的 `build` 闭包提为包级 `buildBin(t, root, target, pkg)`，原调用点同步替换，不改其余断言）：

```go
// buildBin 构建 go 包为二进制（包相对路径以仓库根为基准）。
func buildBin(t *testing.T, root, target, pkg string) {
	t.Helper()
	c := exec.Command("go", "build", "-o", target, pkg)
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("构建 %s 失败: %v\n%s", pkg, err, out)
	}
}

// TestProfileKnownDifference M2 验收：--fix-a 的 A（全通过）与空配置 B
// （半通过）镜像 2 局后，profile 子命令查得 A 的 correctness 分严格高于 B。
// 独立起服（与 TestPlatformLoopEcho 隔离，避免共享天梯/画像状态）。
func TestProfileKnownDifference(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过黑盒 E2E")
	}
	root := findRepoRoot(t)
	bin := filepath.Join(t.TempDir(), "agentbattle.exe")
	buildBin(t, root, bin, "./runner/cmd/agentbattle")
	serverBin := filepath.Join(t.TempDir(), "agentbattle-server.exe")
	buildBin(t, root, serverBin, "./platform/cmd/agentbattle-server")

	tasksDir := t.TempDir()
	if err := copyDir(filepath.Join(root, "examples", "fix-add"), filepath.Join(tasksDir, "fix-add")); err != nil {
		t.Fatalf("拷贝示例任务失败: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "profile-e2e.db")
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

	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("cli %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	tokA := extractToken(t, run("register", "--server", "http://"+addr, "--name", "profA"))
	tokB := extractToken(t, run("register", "--server", "http://"+addr, "--name", "profB"))

	taskOut := filepath.Join(t.TempDir(), "task")
	run("fetch", "--server", "http://"+addr, "--task", "fix-add", "--out", taskOut)

	rep := run("mirror",
		"--server", "http://"+addr,
		"--task", taskOut,
		"--task-id", "fix-add",
		"--agent", "echo",
		"--fix-a", "add() { echo $(( $1 + $2 )); }",
		"--rounds", "2",
		"--name-a", "profA", "--token-a", tokA,
		"--name-b", "profB", "--token-b", tokB,
		"--out", filepath.Join(t.TempDir(), "reports"))
	if !strings.Contains(rep, "A崩 0 | B崩 0") {
		t.Fatalf("mirror 存在崩溃侧:\n%s", rep)
	}

	// profile 子命令：A correctness 100（2/2 在 [1,1,.5,.5] 基线中 pct=75、
	// AllPass 同为 75 → 均值 75）；B 25。断言严格高于即可（对公式细节鲁棒）。
	getScore := func(name string) float64 {
		t.Helper()
		out := run("profile", "--server", "http://"+addr, "--name", name)
		re := regexp.MustCompile(`(?m)^correctness\t([\d.]+)\t`)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("%s 输出缺 correctness 行:\n%s", name, out)
		}
		s, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("correctness 分数解析失败: %v\n%s", err, out)
		}
		return s
	}
	sa, sb := getScore("profA"), getScore("profB")
	if sa <= sb {
		t.Fatalf("画像未复现已知差异: A=%v B=%v\n", sa, sb)
	}
	if sa < 70 || sb > 30 {
		t.Fatalf("分数偏离百分位公式预期（A≈75 B≈25）: A=%v B=%v", sa, sb)
	}
}
```

（`TestPlatformLoopEcho` 内原 `build := func(target, pkg string) {...}` 闭包替换为 `buildBin(t, root, bin, ...)` / `buildBin(t, root, serverBin, ...)` 两次调用。）

- [ ] **Step 2: 跑测试确认通过（本步为新测试直接写+验证，无须先红——它依赖 Task 1-6 的全部产出，红已在各任务内验证过）**

```bash
go test -race -run 'TestPlatformLoopEcho|TestProfileKnownDifference' ./runner/e2e -count=1 -v
```

预期：两测试 PASS（约 10-15s）。若 `TestProfileKnownDifference` 中 A correctness 为 75 而断言 `sa < 70` 失败，检查镜像局数与基线池构成（2 局 × 双侧 = 基线 4 条）。

- [ ] **Step 3: 全量回归**

```bash
go vet ./... && go test -race ./... -count=1 2>&1 | tail -15
```

预期：12 包全绿（judge `TestRunCommandTimeout` 存量抖动除外——单独复跑 3 次确认后按已知问题记录，不阻塞）。

- [ ] **Step 4: 更新 CHANGE.md**

顶部（`# CHANGE.md — 项目迭代记录` 标题行之后）追加：

```markdown
## 2026-09-13 · M2 计划 1 完成：六维能力画像管道

**主题**：平台在每场对局结算后自动重建 agent 六维能力画像（0-100），API 与 CLI 可查，E2E 验收"画像复现已知差异"通过

**核心变更**：
- store：matches 加 task_type 列（旧库 ALTER 迁移，缺省 general）+ agent_profiles 覆盖式 upsert 表 + 自身窗口（agent × task_type 近 50 局）与归一化基线（task_type 全体近 200 局）两个查询
- 新包 platform/internal/profile：单局指标提取（事件流 + 判分结果 → 六维原始指标，空流/除零/NaN 有界）→ 全体池百分位归一化（低好指标取反、并列取中档、空基线取 50）→ 覆盖落库；缺席规则有定义（无错误局调试维满分、无 edit 局规划维零样本、无事件流局不参与工具/成本维）
- 六维口径（纯规则，零 LLM 依赖）：正确性（通过率+全过率）、调试（报错恢复率）、工具效率（调用量+无效调用率）、成本（tokens+耗时）、规划（前置探查比）、稳定性（崩溃率+通过率偏离度）；崩溃判定 = total==0（runner 0/0 上报既有契约）
- api：task_type 从任务包 task.json 读入（缺字段归一 general）；settle 后同步重算双方画像（失败 log 降级不阻断结算，下局自愈）；GET /api/agents/{name}/profile（404 与空画像区分）
- runner CLI：profile 子命令（tabwriter 六维表，低样本标注）；规格修订——归一化基线必须是全体池而非自身窗口（否则均匀表现恒约 50 分，无法区分好坏）
- E2E 验收 TestProfileKnownDifference：--fix-a 全过 vs 空配置半过镜像 2 局 → A correctness 75 > B 25

**遗留事项**：
- 复盘报告、--dry-run 影子赛（M2 其余两件）后续规格
- 画像归一化基线在小样本任务类型下波动大（E2E 用 4 局基线）；任务类型池扩大后自然收敛
- 六维中"工具选择合理性"现为规则近似，M3+ LLM judge 接管
```

- [ ] **Step 5: 更新 CLAUDE.md 进度行**

CHANGE.md 链接条目改为：

```markdown
- [CHANGE.md](./CHANGE.md) — 迭代记录。当前进度：**M1（计划 1/2/3）与 M2 计划 1（六维能力画像管道）均已完成**——平台注册/任务下发/结果上报/Elo 结算/天梯全链路联通，并发与孤儿对局已加固；结算后自动重建六维能力画像（全体池百分位归一化），`profile` 子命令与 API 可查。复盘报告与 --dry-run 属 M2 后续。平台/Runner 代码分别在 `platform/`、`runner/` 子树。
```

- [ ] **Step 6: 提交并合并**

```bash
git add runner/e2e/ CHANGE.md CLAUDE.md
git commit -m "test(e2e)+docs: 画像复现已知差异验收 + M2 计划1 完成记录"
git checkout main && git merge --ff-only m2/profile
```

（push 由用户确认后执行：`git push origin main m2/profile`。）

---

## Self-Review 记录

1. **规格覆盖**：§2 五项口径决策 → 全部落进任务（平台侧计算=架构；纯规则=六维口径；同步重算=Task 5 钩子；CLI+API=Task 5/6；task_type=Task 1/5）；§3 六维表 → Task 2/3 逐维实现；§4 数据流 → Task 4/5；§5 组件 → Task 1-6 一一对应；§6 错误处理 → 降级（decodeEvents nil）、幂等（覆盖 upsert + Task 4 测试）、NaN 有界（Task 2 测试）、404/空区分（Task 5 测试）；§7 测试 → 各任务步骤 + Task 7 E2E 验收；§8 红线 → 画像仅消费已回传事件流，无新采集。无遗漏。
2. **占位符扫描**：所有步骤含完整代码/命令/预期；无 TBD/类似 Task N。（Task 6 client_test 需按既有 import 微调、Task 1 Step 5 需 grep 定位调用点——均已给出明确操作，非占位。）
3. **类型一致性**：`MatchMetrics` 字段（PassRatio/AllPass/HasErrors/Recovery/ToolCalls/ErrRatio/HasEdit/PreEdit/Tokens/WallMS/Crash/HasEvents）在 Task 2/3/4 一致；`Profile`/`Dim` JSON 键（score/raw/sample、sample_size/low_sample/dims）在 Task 4 序列化、Task 5 断言、Task 6 解析三处一致；`store.ProfileMatch` 四字段与 Task 4 消费一致；`CreateMatch(taskID, taskType string, agentA, agentB int64)` 新签名在 Task 1/5/7 与 store_test 更新说明一致；`pct` 语义（75/25/空基线 50）在 Task 3 定义并被 Task 3/7 断言引用一致。

## Out of scope

- 复盘报告、--dry-run 影子赛（M2 其余两件，独立规格）
- LLM judge（工具选择合理性现为规则近似；M3+）
- 按 task_type 分榜的天梯改造（六维已按 task_type 分池，天梯分榜另行规划）
- 画像历史快照/趋势曲线（现仅保留最新覆盖态；成长/退化可视化属产品化阶段）
