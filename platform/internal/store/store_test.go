package store

import (
	"path/filepath"
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
