// Package store 是平台 M1 的 SQLite 持久层（modernc.org/sqlite 纯 Go 驱动）。
// M1 验证定位；PostgreSQL 迁移在后续里程碑，收敛在本包内替换。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"agentbattle/platform/internal/elo"
)

type Store struct{ db *sql.DB }

// schema：agents/matches/results 三表。results 以 (match_id, side) 为主键，
// 同一侧重复上报天然被主键拒绝。外键约束通过 DSN 的 _pragma 参数
//（foreign_keys(1)）开启——SQLite 默认关闭外键，不开启的话向不存在的
// matchID/agentID 写入会被静默接受。
const schema = `
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

// Open 打开（必要时创建）SQLite 库并建表。
// foreign_keys 经 DSN _pragma 开启，对连接池内每条新建连接都生效。
func Open(path string) (*Store, error) {
	// file: URI 中路径统一用 / 分隔（Windows 兼容）；foreign_keys 开启外键；
	// busy_timeout(5000) 让并发写锁竞争退避等待最多 5s 而非立即 SQLITE_BUSY
	//（M1 单进程，但 database/sql 连接池天然多连接，结算/上报并发路径可竞争）。
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
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

// Agent 是天梯上的一名 agent 及其累计战绩。
type Agent struct {
	ID                        int64
	Name, Token               string
	Rating                    float64
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

func (s *Store) AgentByName(name string) (Agent, bool, error) {
	a, err := scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE name = ?`, name))
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

// MatchByID 返回对局元信息（供 API 层校验上报方归属）。
func (s *Store) MatchByID(id int64) (taskID string, aID, bID int64, status, winner string, err error) {
	err = s.db.QueryRow(
		`SELECT task_id, agent_a, agent_b, status, winner FROM matches WHERE id = ?`, id).
		Scan(&taskID, &aID, &bID, &status, &winner)
	return
}

// Result 是一侧的对局结果（events_gz 为 NDJSON 事件流 gzip 压缩，可空）。
type Result struct {
	Passed, Total int
	WallMS        int64
	DiffHash      string
	EventsGZ      []byte
}

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

	// 6. 同一事务内落库（结算权已抢占，winner 可安全补写；
	// GET /matches/{id} 经 MatchByID 回读该列）
	if _, err := tx.Exec(`UPDATE matches SET winner = ? WHERE id = ?`, w, matchID); err != nil {
		return err
	}
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

// winnerOf：通过比例高者胜 → 同比例 wall 短者胜 → 平局。双零直接平局
// （失败的耗时没有竞速信号，与 runner 侧判定一致）。
func winnerOf(pa, ta int, wa int64, pb, tb int, wb int64) string {
	if pa == 0 && pb == 0 {
		return "tie"
	}
	sa, sb := ratio(pa, ta), ratio(pb, tb)
	if sa > sb {
		return "a"
	}
	if sa < sb {
		return "b"
	}
	switch {
	case wa < wb:
		return "a"
	case wb < wa:
		return "b"
	}
	return "tie"
}

// ratio 计算通过比例，Total 为 0（无测试）时记 0。
func ratio(p, t int) float64 {
	if t == 0 {
		return 0
	}
	return float64(p) / float64(t)
}

// Ladder 返回天梯：按 rating 降序，同分按局数多者在前。
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
