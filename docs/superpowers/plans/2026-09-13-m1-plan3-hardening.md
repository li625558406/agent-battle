# M1 计划 3：平台加固收尾 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 清除 M1 计划 2 收官审查遗留的平台侧风险（并发写锁、结算竞态、孤儿对局），并为 E2E 提供确定性判分差（CLI 分侧配置），为 M2 能力分析里程碑清障。

**Architecture:** 全部改动落在既有分层内：`platform/internal/store`（busy_timeout、settle 原子幂等、孤儿清理）→ `platform/internal/api`（aborted 对局拒绝上报）→ `platform/cmd/agentbattle-server`（周期清理 ticker）→ `runner/cmd/agentbattle`（`--fix-a/--fix-b` 分侧 echo 配置）→ `runner/e2e`（三分支断言升级为确定性断言）。无新依赖、无 schema 破坏性变更。

**Tech Stack:** Go 1.25 标准库 + modernc.org/sqlite（唯一第三方依赖）。

**Out of scope（明确不做，勿实现）：**
- **judge_key 按局随机下发**：与任务包预签名机制冲突——runner 侧 `judge.VerifyDir` 要求使用与 `tests/manifest.json` 签名时相同的 key，任务包在打包时一次性签名；按局随机 key 会使验签必挂。要支持它需要平台按局重签任务包或平台侧判分，属 M2 防作弊改造，不在本计划。
- sentinel 类型化判别（mirror.go 的 `errors.Is(Canceled/DeadlineExceeded)`）：仅扩展第三方 adapter 前需要，YAGNI。
- bundle 路由鉴权、judge `GIT_INDEX_FILE` 过滤：维持 backlog。

---

## 工程上下文（执行前必读）

- 仓库根：`D:\AI\agent-battle`（bash 路径 `/d/AI/agent-battle`），单 Go module `agentbattle`，`platform/` 与 `runner/` 为子目录。
- 所有测试/构建命令在**仓库根**执行：`go test -race ./...`（module 根即仓库根）。
- 黑盒 E2E 是 `runner/e2e/platform_e2e_test.go` 的 `TestPlatformLoopEcho`，`-short` 模式会跳过；单独跑它：`go test -race -run TestPlatformLoopEcho ./runner/e2e -v`（约 4s）。
- **IDE gopls 在本仓库有大量滞后误报**（`undefined: X`、UnusedImport、structtag 假告警）。一切以真实 `go build ./... && go vet ./... && go test -race ./...` 为准，不要因 IDE 诊断改代码。
- 提交信息用 conventional commits（`feat(store): ...`、`fix(api): ...`），与既有历史一致。
- Go internal 边界：`runner/internal/...` 只能被 `runner/...` 导入；platform 不能复用 runner 的 winner 实现——这是 `store.winnerOf` 与 `session.winner` 双份并存的原因，改胜负规则必须双侧同步（本计划不改规则）。
- 已有测试助手：`platform/internal/store/store_test.go` 的 `openTest(t)`（store 是 internal 测试包，可直接访问 `s.db`）；`platform/internal/api/api_test.go` 的 `newServer(t)`/`do(t,...)`/`register(t,...)`；`runner/internal/session/mirror_test.go` 的 `newTask(t)`（签好名的临时任务，1 个判分用例）。
- echo adapter 的确定性"解法"内容（写入沙箱 calc.sh，对 examples/fix-add 判 2/2）：`add() { echo $(( $1 + $2 )); }`（单行合法 bash，session 测试已在用同款）。

---

### Task 1: store busy_timeout（并发写锁容忍）

SQLite 默认 `busy_timeout=0`：两个连接并发写时后者立即报 `SQLITE_BUSY`。DSN 追加 `_pragma=busy_timeout(5000)`，让写锁竞争退避等待而非直接失败——这是 Task 2 并发结算测试的前提。

**Files:**
- Modify: `platform/internal/store/store.go`（`Open` 函数，约 59-71 行）
- Test: `platform/internal/store/store_test.go`

- [ ] **Step 0: 建分支**

```bash
cd /d/AI/agent-battle && git checkout main && git pull origin main && git checkout -b m1/hardening
```

- [ ] **Step 1: 写失败测试**

在 `platform/internal/store/store_test.go` 追加（import 区补 `"time"`）：

```go
// TestBusyTimeoutAllowsConcurrentWriters 验证 busy_timeout：一个连接持写锁
// 期间，另一连接的写应退避等待锁释放后成功，而不是立即 SQLITE_BUSY 失败。
func TestBusyTimeoutAllowsConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s1.Close() })
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })

	// s1 开启写事务并持锁（INSERT 触发 RESERVED 锁）
	tx, err := s1.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO agents (name, token, created_at) VALUES ('lock', 'tok-lock', 0)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s2.CreateAgent("waiter")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("并发写应在锁释放后成功（busy_timeout 生效）: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race -run TestBusyTimeoutAllowsConcurrentWriters ./platform/internal/store -v
```

预期：FAIL，错误为 SQLITE_BUSY（`database is locked`）——200ms 远小于写事务持锁时长之外，无 busy_timeout 时 s2 立即失败。

- [ ] **Step 3: 最小实现**

`platform/internal/store/store.go` 的 `Open` 中，DSN 一行改为：

```go
	// file: URI 中路径统一用 / 分隔（Windows 兼容）；foreign_keys 开启外键；
	// busy_timeout(5000) 让并发写锁竞争退避等待最多 5s 而非立即 SQLITE_BUSY
	//（M1 单进程，但 database/sql 连接池天然多连接，结算/上报并发路径可竞争）。
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race -run TestBusyTimeoutAllowsConcurrentWriters ./platform/internal/store -v
```

预期：PASS。

- [ ] **Step 5: 提交**

```bash
git add platform/internal/store/store.go platform/internal/store/store_test.go
git commit -m "feat(store): DSN 追加 busy_timeout(5000)，并发写退避而非 SQLITE_BUSY"
```

---

### Task 2: settle 原子幂等（事务内条件更新抢占结算权）

现状缺陷：`settle` 在事务**外**读 status 判幂等（`if status == "done" { return nil }`），事务**内**才落 Elo。两个连接同时看到 `pending` 会各自完整结算一次 → Elo/统计双计。修复：把"抢占结算权"改为事务内第一条 `UPDATE matches SET status='done' ... WHERE id=? AND status='pending'`，`RowsAffected==0` 即别人已结算，回滚为 no-op；其余读取全部移入同一事务。

**Files:**
- Modify: `platform/internal/store/store.go`（替换整个 `settle` 函数，约 183-282 行）
- Test: `platform/internal/store/store_test.go`

- [ ] **Step 1: 写失败测试**

在 `platform/internal/store/store_test.go` 追加（import 区补 `"sync"`）：

```go
// TestSettleIdempotent 验证重复 settle 是 no-op：统计与评分不得二次变动。
func TestSettleIdempotent(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	mid, _ := s.CreateMatch("fix-add", a.ID, b.ID)
	if _, err := s.AddResult(mid, "a", Result{Passed: 2, Total: 2, WallMS: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(mid, "b", Result{Passed: 1, Total: 2, WallMS: 50}); err != nil {
		t.Fatal(err)
	}
	ga, _ := s.AgentByID(a.ID)
	if err := s.settle(mid); err != nil {
		t.Fatal(err)
	}
	ga2, _ := s.AgentByID(a.ID)
	if ga2.Games != 1 || ga2.Rating != ga.Rating {
		t.Fatalf("重复结算改动了统计: before games=%d rating=%v, after games=%d rating=%v",
			ga.Games, ga.Rating, ga2.Games, ga2.Rating)
	}
}

// TestSettleConcurrentReports 对抗性：双侧几乎同时上报（两个 goroutine 都
// 可能看到 count==2 并调 settle），每场必须恰好结算一次（games 精确 = 场数）。
func TestSettleConcurrentReports(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	for i := 0; i < 10; i++ {
		mid, err := s.CreateMatch("fix-add", a.ID, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, errs[0] = s.AddResult(mid, "a", Result{Passed: 2, Total: 2, WallMS: 100}) }()
		go func() { defer wg.Done(); <-start; _, errs[1] = s.AddResult(mid, "b", Result{Passed: 1, Total: 2, WallMS: 50}) }()
		close(start)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("并发上报不应失败: %v", err)
			}
		}
	}
	ga, _ := s.AgentByID(a.ID)
	gb, _ := s.AgentByID(b.ID)
	if ga.Games != 10 || gb.Games != 10 {
		t.Fatalf("10 场对局应恰好各结算一次: A games=%d B games=%d", ga.Games, gb.Games)
	}
	if ga.Wins != 10 || gb.Losses != 10 {
		t.Fatalf("胜负统计异常: A wins=%d B losses=%d", ga.Wins, gb.Losses)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race -run "TestSettleIdempotent|TestSettleConcurrentReports" ./platform/internal/store -v -count=1
```

预期：`TestSettleConcurrentReports` FAIL（games > 10，双计），`TestSettleIdempotent` 可能侥幸 PASS（单线程调用旧代码时 status 已是 done）。多跑几次 `-count=3` 提高竞态命中率。

- [ ] **Step 3: 重写 settle**

用下面的实现整体替换 `platform/internal/store/store.go` 的 `settle` 函数（`winnerOf`/`ratio`/`AddResult` 不动）：

```go
// settle 结算一场对局：事务内先以条件更新抢占结算权（仅 status='pending'
// 的对局能被置为 done），抢到的事务完成 Elo 与统计落库；没抢到（并发结算
// 竞态中对方已结算、或对局不存在/已结束）回滚为 no-op。幂等由单条 UPDATE
// 的原子性保证：双侧几乎同时到齐时恰好结算一次，Elo/统计不双计。
// 注意：winner 逻辑在 runner 侧（agentbattle/runner/internal/session/mirror.go
// 的 winner 函数，跨 internal 边界不可导入）与本包 winnerOf 各有一份，
// 规则变更必须双侧同步。
func (s *Store) settle(matchID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 原子抢占结算权：RowsAffected==0 即已被并发结算或对局不存在
	res, err := tx.Exec(
		`UPDATE matches SET status = 'done', settled_at = ? WHERE id = ? AND status = 'pending'`,
		time.Now().Unix(), matchID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return nil
	}

	// 2. 事务内读双侧结果
	rows, err := tx.Query(`SELECT side, passed, total, wall_ms FROM results WHERE match_id = ?`, matchID)
	if err != nil {
		return err
	}
	var pa, ta, pb, tb int
	var wa, wb int64
	for rows.Next() {
		var side string
		var p, t int
		var w int64
		if err := rows.Scan(&side, &p, &t, &w); err != nil {
			rows.Close()
			return err
		}
		if side == "a" {
			pa, ta, wa = p, t, w
		} else {
			pb, tb, wb = p, t, w
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	// 3. 事务内读双方 agent id 与当前数据
	var ida, idb int64
	if err := tx.QueryRow(`SELECT agent_a, agent_b FROM matches WHERE id = ?`, matchID).
		Scan(&ida, &idb); err != nil {
		return fmt.Errorf("读对局 %d: %w", matchID, err)
	}
	agA, err := scanAgent(tx.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, ida))
	if err != nil {
		return fmt.Errorf("读 agent %d: %w", ida, err)
	}
	agB, err := scanAgent(tx.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, idb))
	if err != nil {
		return fmt.Errorf("读 agent %d: %w", idb, err)
	}

	// 4. 判胜并结算 Elo：scoreA a→1 / b→0 / 平→0.5
	w := winnerOf(pa, ta, wa, pb, tb, wb)
	var scoreA float64
	switch w {
	case "a":
		scoreA = 1
	case "b":
		scoreA = 0
	default:
		scoreA = 0.5
	}
	na, nb := elo.Update(agA.Rating, agB.Rating, scoreA, agA.Games, agB.Games)

	// 5. 双方新统计四列
	aGames, aWins, aLosses, aTies := agA.Games+1, agA.Wins, agA.Losses, agA.Ties
	bGames, bWins, bLosses, bTies := agB.Games+1, agB.Wins, agB.Losses, agB.Ties
	switch w {
	case "a":
		aWins++
		bLosses++
	case "b":
		bWins++
		aLosses++
	default:
		aTies++
		bTies++
	}

	// 6. 同一事务内落库
	if _, err := tx.Exec(
		`UPDATE agents SET rating = ?, games = ?, wins = ?, losses = ?, ties = ? WHERE id = ?`,
		na, aGames, aWins, aLosses, aTies, ida); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE agents SET rating = ?, games = ?, wins = ?, losses = ?, ties = ? WHERE id = ?`,
		nb, bGames, bWins, bLosses, bTies, idb); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race -run "TestSettle|TestMatch|TestAddResult|TestLadder" ./platform/internal/store -v -count=2
```

预期：全部 PASS（含既有 TestMatchSettleWin/TestMatchSettleTieAndDoubleZero/TestAddResultRejectsBadSideAndDuplicate/TestAddResultForeignKeys——它们的路径未变，仍经 AddResult → settle）。

- [ ] **Step 5: 提交**

```bash
git add platform/internal/store/store.go platform/internal/store/store_test.go
git commit -m "fix(store): settle 改事务内条件更新抢占结算权，并发上报恰好结算一次"
```

---

### Task 3: 孤儿 waiting match 超时清理

mirror 中止会在平台侧遗留永远 `pending` 的半场对局。三层修复：`store.SweepStaleMatches`（把超时 pending 置为 `aborted`）+ `AddResult`/API 拒绝对非 pending 对局上报 + server 周期清理 ticker。`aborted` 对局不参与 Elo、拒绝后续上报。

**Files:**
- Modify: `platform/internal/store/store.go`（新增 `SweepStaleMatches`；`AddResult` 加状态守卫）
- Modify: `platform/internal/api/api.go`（`handleResult` 加 pending 校验，约 161-176 行）
- Modify: `platform/cmd/agentbattle-server/main.go`（`--match-timeout` flag + ticker goroutine）
- Test: `platform/internal/store/store_test.go`、`platform/internal/api/api_test.go`

- [ ] **Step 1: 写 store 层失败测试**

在 `platform/internal/store/store_test.go` 追加：

```go
// TestSweepStaleMatches 验证孤儿对局清理：超时 pending → aborted，
// aborted 拒绝上报，未超时对局不受影响，二次清扫幂等。
func TestSweepStaleMatches(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	old1, _ := s.CreateMatch("fix-add", a.ID, b.ID)
	old2, _ := s.CreateMatch("fix-add", a.ID, b.ID)
	fresh, _ := s.CreateMatch("fix-add", a.ID, b.ID)

	// old1/old2 的 created_at 拨回 2 小时前（模拟 mirror 中止遗留的孤儿）
	past := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE matches SET created_at = ? WHERE id IN (?, ?)`, past, old1, old2); err != nil {
		t.Fatal(err)
	}

	n, err := s.SweepStaleMatches(30 * time.Minute)
	if err != nil || n != 2 {
		t.Fatalf("应清理 2 场, got n=%d err=%v", n, err)
	}
	// 二次清扫：幂等
	if n, err := s.SweepStaleMatches(30 * time.Minute); err != nil || n != 0 {
		t.Fatalf("二次清理应幂等, got n=%d err=%v", n, err)
	}

	// aborted 对局拒绝上报
	if _, err := s.AddResult(old1, "a", Result{Passed: 1, Total: 1, WallMS: 10}); err == nil {
		t.Fatal("aborted 对局应拒绝上报")
	}
	// 未超时的 fresh 对局不受影响：双侧到齐正常结算
	if _, err := s.AddResult(fresh, "a", Result{Passed: 2, Total: 2, WallMS: 100}); err != nil {
		t.Fatal(err)
	}
	done, err := s.AddResult(fresh, "b", Result{Passed: 1, Total: 2, WallMS: 50})
	if err != nil || !done {
		t.Fatalf("fresh 对局应正常结算: done=%v err=%v", done, err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race -run TestSweepStaleMatches ./platform/internal/store -v
```

预期：编译失败 `s.SweepStaleMatches undefined`。

- [ ] **Step 3: 实现 SweepStaleMatches + AddResult 守卫**

`platform/internal/store/store.go`：`AddResult` 函数替换为（插入前加状态守卫）：

```go
// AddResult 记录一侧结果；双侧到齐时结算 Elo 并更新统计。
// 返回该次写入后对局是否已结算。
// 仅 pending 状态的对局接受上报：aborted（超时清理）与 done（已结算）
// 一律拒绝——孤儿复活或已结算对局被补写都会污染战绩。
// 同侧重复提交被 results 的 (match_id, side) 主键拒绝；不存在的 matchID
// 被外键约束拒绝（Open 已开启 foreign_keys）。
func (s *Store) AddResult(matchID int64, side string, r Result) (bool, error) {
	if side != "a" && side != "b" {
		return false, fmt.Errorf("非法 side: %q", side)
	}
	if _, _, _, status, _, err := s.MatchByID(matchID); err != nil {
		return false, fmt.Errorf("读对局 %d: %w", matchID, err)
	} else if status != "pending" {
		return false, fmt.Errorf("对局 %d 已结束（%s），拒绝上报", matchID, status)
	}
	if _, err := s.db.Exec(
		`INSERT INTO results (match_id, side, passed, total, wall_ms, diff_hash, events_gz)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		matchID, side, r.Passed, r.Total, r.WallMS, r.DiffHash, r.EventsGZ); err != nil {
		return false, fmt.Errorf("写入结果（同侧重复提交会被主键拒绝）: %w", err)
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
```

同文件在 `settle` 之后新增：

```go
// SweepStaleMatches 孤儿对局清理：把 pending 且 created_at 早于
// now-olderThan 的对局置为 aborted（mirror 中止会在平台侧遗留永远 waiting
// 的半场对局）。aborted 不参与 Elo、拒绝后续上报（AddResult 守卫）。
// 返回被清理的对局数。olderThan 为负时 cutoff 在未来，全部 pending 命中
//（测试便利，语义即"清扫一切未结算"）。
func (s *Store) SweepStaleMatches(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := s.db.Exec(
		`UPDATE matches SET status = 'aborted' WHERE status = 'pending' AND created_at <= ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

- [ ] **Step 4: 跑 store 测试确认通过**

```bash
go test -race ./platform/internal/store -v -count=1
```

预期：全部 PASS。注意 `TestAddResultForeignKeys`（向不存在 match 上报）：守卫的 `MatchByID` 对不存在 id 返回 `sql.ErrNoRows` → 现在走"读对局"错误分支而非外键错误，测试只断言 err != nil 即仍 PASS；若该测试断言了具体错误文案，按实际输出修正断言文案为"读对局"分支。

- [ ] **Step 5: 写 api 层失败测试**

`platform/internal/api/api_test.go`：把现有 `newServer` 改造为薄封装（不动 4 个既有调用点），并新增测试（import 区补 `"time"`）：

```go
func newServerWithStore(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tasksDir := t.TempDir()
	// 伪造一个最小任务目录
	if err := os.MkdirAll(filepath.Join(tasksDir, "demo", "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, "demo", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "demo", "task.json"),
		[]byte(`{"task_id":"demo","name":"d","description":"x","timeout_sec":60}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "demo", "tests", "manifest.json"),
		[]byte(`{"task_id":"demo","tests":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "demo", "tests", "sig"), []byte("sig"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, tasksDir, []byte("dev-secret")))
	t.Cleanup(srv.Close)
	return srv, st
}

func newServer(t *testing.T) *httptest.Server {
	srv, _ := newServerWithStore(t)
	return srv
}

// TestResultRejectedAfterSweep 验证 API 层拒绝对 aborted 对局上报（409 固定文案）。
func TestResultRejectedAfterSweep(t *testing.T) {
	srv, st := newServerWithStore(t)
	tokA := register(t, srv, "A")
	tokB := register(t, srv, "B")
	resp, m := do(t, "POST", srv.URL+"/api/matches", tokA,
		map[string]any{"task_id": "demo", "agent_a": "A", "agent_b": "B"})
	if resp.StatusCode != 201 {
		t.Fatalf("创建对局状态码 = %d, 期望 201", resp.StatusCode)
	}
	matchID := strconv.FormatInt(int64(m["match_id"].(float64)), 10)

	// 负阈值：cutoff 在未来，全部 pending 命中 → 该对局被置为 aborted
	if _, err := st.SweepStaleMatches(-time.Minute); err != nil {
		t.Fatal(err)
	}

	resp, m = do(t, "POST", srv.URL+"/api/matches/"+matchID+"/results", tokA,
		map[string]any{"side": "a", "passed": 1, "total": 1, "wall_ms": 100})
	if resp.StatusCode != 409 {
		t.Fatalf("aborted 对局上报状态码 = %d, 期望 409", resp.StatusCode)
	}
	if s, _ := m["error"].(string); s != "对局已结束，拒绝上报" {
		t.Fatalf("aborted 上报错误文案 = %q, 期望固定文案", s)
	}
}
```

- [ ] **Step 6: 跑 api 测试确认失败**

```bash
go test -race -run TestResultRejectedAfterSweep ./platform/internal/api -v
```

预期：FAIL——status 是 200（waiting），因为 handleResult 尚无 pending 校验。

- [ ] **Step 7: api 实现**

`platform/internal/api/api.go` 的 `handleResult`：把第一次 `MatchByID` 调用（约 167 行）改为捕获 status，并在归属校验后加拦截：

```go
	_, aID, bID, status, _, err := s.St.MatchByID(matchID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "对局不存在")
		return
	}
	// 上报方必须是对局双方之一
	if ag.ID != aID && ag.ID != bID {
		writeErr(w, http.StatusForbidden, "当前 token 不属于该对局双方")
		return
	}
	// 已结束（done 已结算 / aborted 超时清理）的对局拒绝上报：
	// 防结算后改写与孤儿复活（对抗性：重复触发/状态耦合）
	if status != "pending" {
		writeErr(w, http.StatusConflict, "对局已结束，拒绝上报")
		return
	}
```

其余不动（既有的同侧重复 409 用例发生在 pending 状态，走 AddResult 主键分支，不受影响）。

- [ ] **Step 8: 跑 api 全包测试确认通过**

```bash
go test -race ./platform/internal/api -v -count=1
```

预期：全部 PASS。

- [ ] **Step 9: server 周期清理**

`platform/cmd/agentbattle-server/main.go`：import 区补 `"time"`，flag 区（`storePath` 之后）追加：

```go
	matchTimeout := flag.Duration("match-timeout", 30*time.Minute, "pending 对局超时清理阈值（0 禁用清理）")
```

`st.Open` 成功之后、监听日志之前追加：

```go
	// 孤儿对局清理：mirror 中止会在平台侧遗留永远 waiting 的半场对局；
	// 周期扫描把超时 pending 对局置为 aborted（不参与 Elo、拒绝后续上报）
	if *matchTimeout > 0 {
		go func() {
			for range time.Tick(time.Minute) {
				n, err := st.SweepStaleMatches(*matchTimeout)
				if err != nil {
					log.Printf("孤儿对局清理失败: %v", err)
				} else if n > 0 {
					log.Printf("已清理 %d 场超时未结算对局", n)
				}
			}
		}()
	}
```

- [ ] **Step 10: 全平台包回归 + 提交**

```bash
go build ./... && go vet ./... && go test -race ./platform/... -count=1
git add platform/internal/store/store.go platform/internal/store/store_test.go platform/internal/api/api.go platform/internal/api/api_test.go platform/cmd/agentbattle-server/main.go
git commit -m "feat(platform): 孤儿 pending 对局超时清理（aborted 状态 + 上报守卫 + server ticker）"
```

---

### Task 4: CLI 分侧配置 --fix-a/--fix-b

现状：CLI mirror 对 echo 双侧注入完全相同的配置，单局结局由 WallMS 抖动决定（E2E 被迫三分支断言）。新增 `--fix-a/--fix-b`：非空时对应侧用 `adapter.Echo{FixContent: fix}`（注入正确解法 → 满分），与空配置侧形成确定性判分差。仅 `--agent echo` 可用。

**Files:**
- Modify: `runner/cmd/agentbattle/mirror.go`（flags、校验、MakeA/MakeB 构造）
- Test: `runner/cmd/agentbattle/cli_test.go`

- [ ] **Step 1: 写失败测试**

在 `runner/cmd/agentbattle/cli_test.go` 追加（确认 import 区已有 `"strings"`，没有则补）：

```go
// TestMirrorFixFlagsRequireEcho 分侧解法注入仅对 echo 有意义：
// 与 claude-code 组合必须在开赛前拦截（配置分化走 --env-a/b）。
func TestMirrorFixFlagsRequireEcho(t *testing.T) {
	err := cmdMirror([]string{"--task", t.TempDir(), "--agent", "claude-code", "--fix-a", "x"})
	if err == nil || !strings.Contains(err.Error(), "--fix-a/--fix-b 仅支持") {
		t.Fatalf("应拒绝 --fix-a 与 claude-code 组合: %v", err)
	}
	err = cmdMirror([]string{"--task", t.TempDir(), "--agent", "claude-code", "--fix-b", "x"})
	if err == nil || !strings.Contains(err.Error(), "--fix-a/--fix-b 仅支持") {
		t.Fatalf("应拒绝 --fix-b 与 claude-code 组合: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test -race -run TestMirrorFixFlagsRequireEcho ./runner/cmd/agentbattle -v
```

预期：编译失败 `--fix-a flag 未定义`（flag 包对未注册 flag 返回 error "flag provided but not defined"，测试不会命中预期文案而 FAIL）。

- [ ] **Step 3: 实现**

`runner/cmd/agentbattle/mirror.go`，三处修改：

① flag 定义区（`tokB` 之后）追加：

```go
	fixA := fs.String("fix-a", "", "A 侧 echo 解法内容（--agent echo 专用，制造确定性判分差）")
	fixB := fs.String("fix-b", "", "B 侧 echo 解法内容（--agent echo 专用）")
```

② 校验区（`*rounds <= 0` 检查之后、`--server` 模式校验之前）追加：

```go
	// 分侧解法注入仅对 echo 有意义（claude-code 的配置分化走 --env-a/b）
	if (*fixA != "" || *fixB != "") && *agent != "echo" {
		return fmt.Errorf("--fix-a/--fix-b 仅支持 --agent echo")
	}
```

③ `MirrorConfig` 构造处（`cfg := session.MirrorConfig{...}`），把 `MakeA: mk, MakeB: mk` 替换为分侧构造：

```go
	// 分侧构造：fix 非空时该侧注入解法（确定性满分），否则维持统一 mk
	side := func(fix string) func() adapter.Adapter {
		if fix == "" {
			return mk
		}
		return func() adapter.Adapter { return adapter.Echo{FixContent: fix} }
	}
	cfg := session.MirrorConfig{
		TaskDir:  *task,
		JudgeKey: sessionDevKey(),
		Rounds:   *rounds,
		OutDir:   outDir,
		EnvA:     []string(envA),
		EnvB:     []string(envB),
		MakeA:    side(*fixA),
		MakeB:    side(*fixB),
	}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test -race ./runner/cmd/agentbattle -v -count=1
```

预期：全部 PASS（含既有 register/fetch/ladder/reportRound 测试——MakeA/MakeB 默认行为不变）。

- [ ] **Step 5: 提交**

```bash
git add runner/cmd/agentbattle/mirror.go runner/cmd/agentbattle/cli_test.go
git commit -m "feat(cli): mirror --fix-a/--fix-b 分侧 echo 解法注入，制造确定性判分差"
```

---

### Task 5: E2E 确定性升级（三分支断言 → winner=a 固定断言）

`--fix-a` 注入正确解法后：A 判 2/2、B 判 0/2，通过比例决胜（1 > 0，且不触发双零平局），winner 恒为 `a`，与 WallMS 抖动无关。E2E 从胜方无关三分支收敛为精确断言，同时消灭 CHANGE.md 中"CLI echo 无差异化"遗留项。

**Files:**
- Modify: `runner/e2e/platform_e2e_test.go`（函数 doc 注释、mirror 调用、结算断言块）

- [ ] **Step 1: 修改 E2E**

`runner/e2e/platform_e2e_test.go`：

① 函数 doc 注释（第 20-33 行）整体替换为：

```go
// TestPlatformLoopEcho 平台黑盒全链路：起 server 进程 → CLI 二进制注册
// A/B → fetch 任务包 → mirror --server（echo agent，1 轮）→ 断言结算与
// 天梯可见。
//
// 因 Go internal 边界（platform 不能 import runner/internal），本测试不做
// 进程内组装，全部通过 exec 真实二进制完成——CLI 输出格式（register 的
// "token: " 行、mirror 的结算行、ladder 表格）本身即被测契约。
//
// 确定性说明：--fix-a 给 A 侧注入正确解法（判 2/2），B 侧空配置（判 0/2），
// 通过比例决胜 → winner=a 恒定，与 WallMS 抖动无关。断言覆盖：无崩溃、
// 定级赛结算分（1220/1180）、天梯可见且统计四列与结算交叉一致（防
// runner 侧 winner 与平台侧 winnerOf 两套规则漂移）。
```

② mirror 调用（`rep := run("mirror", ...)`）追加 `--fix-a` 参数（`"--agent", "echo",` 之后插入一行）：

```go
		"--agent", "echo",
		"--fix-a", "add() { echo $(( $1 + $2 )); }",
		"--rounds", "1",
```

③ 结算断言块（`// 结算行：...` 注释起至 `switch` 结束，原 126-150 行）替换为：

```go
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
```

④ 天梯交叉校验块中，把 `gotStatA := fmt.Sprintf("%d %d %d", rowWin.wins, rowWin.losses, rowWin.ties)` 一行起至函数结尾保持不变（`wantAi/wantBi`、rating 对比、games 对比、`gotStatA/gotStatB` 对 `wantStatA/wantStatB` 对比原样保留——它们引用的常量名与上面 const 块一致）。确认无 `wantAi`/`wantBi` 之外的 switch 残留变量（原 `switch winner` 已整体删除）。

- [ ] **Step 2: 跑 E2E 确认通过**

```bash
go test -race -run TestPlatformLoopEcho ./runner/e2e -v -count=1
```

预期：PASS，耗时约 4s。连跑 3 次确认无抖动：`-count=3`。

- [ ] **Step 3: 提交**

```bash
git add runner/e2e/platform_e2e_test.go
git commit -m "test(e2e): --fix-a 注入确定性判分差，三分支断言收敛为 winner=a 精确断言"
```

---

### Task 6: 全量回归 + 文档

**Files:**
- Modify: `CHANGE.md`（顶部追加条目）
- Modify: `CLAUDE.md`（进度行）

- [ ] **Step 1: 全量回归**

```bash
go vet ./... && go test -race ./... -count=1
```

预期：13 包全绿（judge 的 `TestRunCommandTimeout` 存量偶发抖动除外——若它挂，单独复跑 `go test -race -run TestRunCommandTimeout ./runner/internal/judge -count=3` 确认稳定后按存量已知问题记录，不阻塞）。

- [ ] **Step 2: 更新 CHANGE.md**

在 `CHANGE.md` 顶部（`# CHANGE.md` 标题行之后）追加：

```markdown
## 2026-09-13 · M1 计划 3 完成：平台加固收尾 + E2E 确定性升级

**主题**：并发写锁、结算竞态、孤儿对局三类平台风险清除；CLI 分侧配置使 E2E 从三分支断言收敛为确定性断言

**核心变更**：
- store DSN 追加 `_pragma=busy_timeout(5000)`：并发写锁竞争退避等待而非立即 SQLITE_BUSY（database/sql 连接池天然多连接，上报/结算并发路径可竞争）
- settle 原子幂等：事务内以条件更新抢占结算权（`UPDATE matches SET status='done' WHERE id=? AND status='pending'`，RowsAffected==0 即 no-op），全部读取移入同一事务——并发上报双侧时恰好结算一次，Elo/统计不双计（并发 AddResult 测试 10 场 × 2 goroutine 验证）
- 孤儿对局清理：`SweepStaleMatches(olderThan)` 把超时 pending 置为 aborted（不参与 Elo）；AddResult 与 API 层双重守卫拒绝对非 pending 对局上报（409 固定文案）；server `--match-timeout`（默认 30m）周期清扫，mirror 中止遗留的永远 waiting 半场对局得以收敛
- CLI mirror 新增 `--fix-a/--fix-b`：分侧 echo 解法注入（仅 --agent echo），与空配置侧形成确定性判分差
- E2E 升级：`--fix-a` 注入后 winner=a 恒定，胜方无关三分支断言（1220/1180/双 1200）收敛为精确断言
- judge_key 按局随机明确移出 backlog（与任务包预签名机制冲突，VerifyDir 要求与 manifest 签名相同的 key；属 M2 防作弊改造范畴，见计划文档 Out of scope）

**遗留事项**：
- sentinel 类型化判别（扩展第三方 adapter 前置）、bundle 路由鉴权、judge GIT_INDEX_FILE 过滤维持 backlog
- judge `TestRunCommandTimeout` 全量并发下偶发计时抖动（存量，独立复跑稳定）
```

- [ ] **Step 3: 更新 CLAUDE.md 进度行**

`CLAUDE.md` 中 CHANGE.md 链接条目改为：

```markdown
- [CHANGE.md](./CHANGE.md) — 迭代记录。当前进度：**M1 计划 1（Runner 核心闭环）、计划 2（平台最小功能 + Runner 联网对接）、计划 3（平台加固收尾）均已完成**——平台注册/任务下发/结果上报/Elo 结算/天梯全链路联通，并发结算幂等与孤儿对局清理已加固，E2E 确定性验证通过。平台/Runner 代码分别在 `platform/`、`runner/` 子树。
```

- [ ] **Step 4: 提交并合并**

```bash
git add CHANGE.md CLAUDE.md
git commit -m "docs: M1 计划3（平台加固收尾）完成记录"
git checkout main && git merge --ff-only m1/hardening
```

（push 由用户确认后执行：`git push origin main m1/hardening`。）

---

## Self-Review 记录

1. **覆盖度**：收官审查首批建议（busy_timeout、settle 原子幂等）→ Task 1/2；CHANGE.md 孤儿对局遗留 → Task 3；CLI 分侧配置遗留 → Task 4/5（E2E 三分支收敛是它的直接下游）；judge_key 按局随机经分析移出（Out of scope 有论证）。无遗漏。
2. **占位符扫描**：所有步骤含完整代码/命令/预期输出，无 TBD/类似 Task N。
3. **类型一致性**：`SweepStaleMatches(olderThan time.Duration) (int64, error)` 在 Task 3 定义并在 api_test/server 使用一致；`side(fix string) func() adapter.Adapter` 与 `MirrorConfig.MakeA/MakeB` 签名一致；E2E const 块变量名（wantA/wantB/wantStatA/wantStatB/winnerName/loserName）与既有交叉校验代码引用一致。
