package store

import (
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
	if ga.Rating != 1200 || gb.Rating != 1200 || ga.Ties != 1 || gb.Ties != 1 {
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
		mid, err := s.CreateMatch("fix-add", a.ID, b.ID)
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
