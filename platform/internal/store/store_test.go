package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	mid, err := s.CreateMatch("fix-add", "general", a.ID, b.ID)
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
	mid, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)
	_, _ = s.AddResult(mid, "a", Result{Passed: 0, Total: 2, WallMS: 10})
	_, err := s.AddResult(mid, "b", Result{Passed: 0, Total: 2, WallMS: 999})
	if err != nil {
		t.Fatal(err)
	}
	ga, _ := s.AgentByID(a.ID)
	gb, _ := s.AgentByID(b.ID)
	// 双方 0 通过 → 平局（与 runner 侧 winner 规则一致）
	if ga.Rating != 1200 || gb.Rating != 1200 || ga.Ties != 1 || gb.Ties != 1 {
		t.Fatalf("double-zero must tie: %+v %+v", ga, gb)
	}
}

func TestLadderOrder(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("winner")
	b, _ := s.CreateAgent("loser")
	mid, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)
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
	mid, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)
	if _, err := s.AddResult(mid, "c", Result{}); err == nil {
		t.Fatal("bad side must fail")
	}
	if _, err := s.AddResult(mid, "a", Result{Total: 1}); err != nil {
		t.Fatalf("first a: %v", err)
	}
	if _, err := s.AddResult(mid, "a", Result{Total: 1}); err == nil {
		t.Fatal("duplicate side must fail")
	}
}

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

// 对抗性用例：向不存在的 matchID 上报必须失败（Open 开启了 foreign_keys，
// SQLite 默认关闭外键会静默接受脏数据，此处固化该行为不被回归）。
func TestAddResultForeignKeys(t *testing.T) {
	s := openTest(t)
	if _, err := s.AddResult(99999, "a", Result{Total: 1}); err == nil {
		t.Fatal("nonexistent matchID must fail (foreign_keys=ON)")
	}
}

// TestSettleIdempotent 验证重复 settle 是 no-op：统计与评分不得二次变动。
func TestSettleIdempotent(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	mid, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)
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
		mid, err := s.CreateMatch("fix-add", "general", a.ID, b.ID)
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

// TestSweepStaleMatches 验证孤儿对局清理：超时 pending → aborted，
// aborted 拒绝上报，未超时对局不受影响，二次清扫幂等。
func TestSweepStaleMatches(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	old1, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)
	old2, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)
	fresh, _ := s.CreateMatch("fix-add", "general", a.ID, b.ID)

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

// TestSettleParallelMatches 对抗性：多场对局结算真并行（两个 goroutine 各自
// 补上不同 match 的 b 侧，触发多场 settle 同时进行）。settle 的"读 agents →
// 算 → 写 agents"若非原子，并发结算会互相覆盖（丢失更新）：N 场并行结算后
// games 必须精确 = N。旧实现在此用例下 30 场仅剩 16 场统计（红测证据）。
func TestSettleParallelMatches(t *testing.T) {
	const n = 30
	s := openTest(t)
	a, _ := s.CreateAgent("A")
	b, _ := s.CreateAgent("B")
	ids := make([]int64, n)
	for i := range ids {
		mid, err := s.CreateMatch("fix-add", "general", a.ID, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = mid
		// 每场只写 a 侧，b 侧留到并发阶段（避免串行提前结算）
		if _, err := s.AddResult(mid, "a", Result{Passed: 2, Total: 2, WallMS: 100}); err != nil {
			t.Fatal(err)
		}
	}
	// 多路 goroutine 并发上报 b 侧 → 多场结算真并行
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, mid := range ids {
		wg.Add(1)
		go func(i int, mid int64) {
			defer wg.Done()
			_, errs[i] = s.AddResult(mid, "b", Result{Passed: 1, Total: 2, WallMS: 50})
		}(i, mid)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("match %d 并发上报不应失败: %v", i, err)
		}
	}
	ga, _ := s.AgentByID(a.ID)
	gb, _ := s.AgentByID(b.ID)
	if ga.Games != n || gb.Games != n {
		t.Fatalf("丢失更新: %d 场并行结算后 A games=%d B games=%d (应均为 %d)", n, ga.Games, gb.Games, n)
	}
	if ga.Wins != n || gb.Losses != n {
		t.Fatalf("胜负统计异常: A wins=%d B losses=%d (应均为 %d)", ga.Wins, gb.Losses, n)
	}
}

// ---------- M2 画像：task_type 列与 agent_profiles ----------

// TestMatchTaskType 验证 task_type 经 CreateMatch 落库并可经 TaskTypeOf 回读；
// 空串落库时归一为 general。
func TestMatchTaskType(t *testing.T) {
	s := openTest(t)
	// 适配：库开启 foreign_keys，硬编码 agent id(1,2) 会触发外键约束，
	// 故创建真实 agent 后用其 ID。
	a, _ := s.CreateAgent("fk1")
	b, _ := s.CreateAgent("fk2")
	id1, err := s.CreateMatch("task-x", "debug", a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.CreateMatch("task-y", "", a.ID, b.ID)
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

// TestMigrateLegacyMatchesWithoutTaskType 旧库（无 task_type 列）经 Open
// 迁移后 TaskTypeOf 可用、CreateMatch 可落 task_type。
func TestMigrateLegacyMatchesWithoutTaskType(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")
	// 用不经过 Open 迁移逻辑的裸连接建 M1 时期的旧 schema
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `
CREATE TABLE IF NOT EXISTS agents (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT UNIQUE NOT NULL,
	token      TEXT UNIQUE NOT NULL,
	rating     REAL NOT NULL DEFAULT 1200,
	games      INTEGER NOT NULL DEFAULT 0,
	wins       INTEGER NOT NULL DEFAULT 0,
	losses     INTEGER NOT NULL DEFAULT 0,
	ties       INTEGER NOT NULL DEFAULT 0,
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
	if _, err := raw.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open 应完成迁移而非报错: %v", err)
	}
	defer s.Close()
	a, _ := s.CreateAgent("m1a")
	b, _ := s.CreateAgent("m1b")
	mid, err := s.CreateMatch("tk", "debug", a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tt, _ := s.TaskTypeOf(mid); tt != "debug" {
		t.Fatalf("迁移后 task_type 应可用, got %q", tt)
	}
}

// TestProfileWindowAndBaseline 验证自身窗口（按 agent+task_type 过滤、仅
// done、按结算时间倒序）与基线（该 task_type 全体 agent）的过滤语义。
func TestProfileWindowAndBaseline(t *testing.T) {
	s := openTest(t)
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
		if _, err := s.AddResult(mid, side, Result{
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

	// 拨 m1 的 settled_at 到未来一小时：若实现错用 created_at/id 为主排序
	//（m1 的 id 更小），own[0] 仍会是 m2，本断言即红——真正锁定
	// "按结算时间倒序" 语义。同理由 id DESC 兜底同秒并列。
	if _, err := s.db.Exec(`UPDATE matches SET settled_at = ? WHERE id = ?`,
		time.Now().Add(time.Hour).Unix(), m1); err != nil {
		t.Fatal(err)
	}
	// 孤儿对局（aborted）不得进入窗口与基线：m4 只写 a 侧保持 pending，
	// 经负阈值清扫置为 aborted（此时它已有 a 侧 results 行，若过滤失效
	// 会以 1 条混入 own 与 base，断言即红）。双侧都写会让 m4 直接结算为
	// done，清扫便无从命中，aborted 过滤未被真正检验。
	m4 := mk("debug", a1.ID, a2.ID)
	res(m4, "a", nil)
	if _, err := s.SweepStaleMatches(-time.Minute); err != nil { // 负阈值：清扫一切 pending
		t.Fatal(err)
	}
	if _, err := s.AddResult(m4, "b", Result{Passed: 1, Total: 2}); err == nil {
		t.Fatal("aborted 对局应拒绝上报（m4 未被清扫则本测试前提不成立）")
	}

	own, err := s.ProfileWindow(a1.ID, "debug", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 {
		t.Fatalf("a1 debug 窗口应 2 局（general 与 aborted 不计入）, got %d", len(own))
	}
	// m1 双侧 events_gz 均为 nil、m2 的 a 侧为 "gz2"：settled_at 更新的 m1 应在前
	if own[0].EventsGZ != nil {
		t.Fatalf("settled_at 更新的 m1 应排在前: got %q", own[0].EventsGZ)
	}
	if string(own[1].EventsGZ) != "gz2" {
		t.Fatalf("own[1] 应为 m2（events_gz=gz2）, got %q", own[1].EventsGZ)
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
	s := openTest(t)
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

// TestReviewData 双侧 JOIN 取数、缺行零值兜底、不存在对局报错。
func TestReviewData(t *testing.T) {
	s := openTest(t)
	a1, _ := s.CreateAgent("r1")
	a2, _ := s.CreateAgent("r2")

	// 已结算对局：双侧齐全
	m1, err := s.CreateMatch("tk", "general", a1.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(m1, "a", Result{Passed: 2, Total: 2, WallMS: 100, EventsGZ: []byte("gza")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(m1, "b", Result{Passed: 1, Total: 2, WallMS: 200, EventsGZ: []byte("gzb")}); err != nil {
		t.Fatal(err)
	}
	sa, sb, err := s.ReviewData(m1)
	if err != nil {
		t.Fatal(err)
	}
	if sa.AgentID != a1.ID || sa.Agent != "r1" || sa.Passed != 2 || sa.Total != 2 ||
		sa.WallMS != 100 || string(sa.EventsGZ) != "gza" {
		t.Fatalf("sideA 不符: %+v", sa)
	}
	if sb.AgentID != a2.ID || sb.Agent != "r2" || sb.Passed != 1 || sb.Total != 2 ||
		sb.WallMS != 200 || string(sb.EventsGZ) != "gzb" {
		t.Fatalf("sideB 不符: %+v", sb)
	}

	// pending 对局只有 a 侧：b 侧 agent 名照常、结果零值（LEFT JOIN 兜底）
	m2, err := s.CreateMatch("tk", "general", a1.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResult(m2, "a", Result{Passed: 1, Total: 2}); err != nil {
		t.Fatal(err)
	}
	if _, sb, err = s.ReviewData(m2); err != nil {
		t.Fatal(err)
	}
	if sb.Agent != "r2" || sb.Passed != 0 || sb.Total != 0 || sb.WallMS != 0 || sb.EventsGZ != nil {
		t.Fatalf("缺行侧应零值兜底: %+v", sb)
	}

	// 不存在的对局 → 错误
	if _, _, err := s.ReviewData(99999); err == nil {
		t.Fatal("不存在的对局应报错")
	}
}
