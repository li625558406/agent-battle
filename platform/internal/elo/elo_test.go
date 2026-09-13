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
	// 平局：同分不变；强 vs 弱时强方应失分
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
	// 同 K 时零和
	na, nb := Update(1300, 1100, 1, 50, 50)
	delta := (na - 1300) + (nb - 1100)
	if !almostEqual(delta, 0) {
		t.Fatalf("same-K update must be zero-sum, got %v", delta)
	}
	// 跨 K 时各自按自己 K 波动，强方赢弱方收益受 Expected 抑制
	na, nb = Update(1300, 1100, 1, 50, 50)
	if na <= 1300 || nb >= 1100 {
		t.Fatalf("winner must gain, loser must drop: %v/%v", na, nb)
	}
}
