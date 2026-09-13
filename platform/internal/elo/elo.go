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
