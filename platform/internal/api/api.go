// Package api 是平台 M1 的 HTTP 服务：注册 / 对局创建 / 结果上报与 Elo 结算 /
// 天梯 / 任务包 zip 分发。认证使用 X-Token 请求头，token 由注册接口发放，
// 绝不出现在任何非注册响应（尤其天梯）中。
package api

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agentbattle/platform/internal/store"
)

// Server 承载平台 M1 的全部路由。St 为持久层；TasksDir 为任务目录根；
// JudgeKey 为裁判密钥（创建对局时下发给双方 runner 用于 judge 验签）。
type Server struct {
	St       *store.Store
	TasksDir string
	JudgeKey []byte
}

// New 装配 mux 并返回 http.Handler。
func New(st *store.Store, tasksDir string, judgeKey []byte) http.Handler {
	s := &Server{St: st, TasksDir: tasksDir, JudgeKey: judgeKey}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents", s.handleRegisterAgent)
	mux.Handle("POST /api/matches", s.auth(s.handleCreateMatch))
	mux.Handle("POST /api/matches/{id}/results", s.auth(s.handleResult))
	mux.HandleFunc("GET /api/ladder", s.handleLadder)
	mux.HandleFunc("GET /api/tasks/{id}/bundle", s.handleBundle)
	return mux
}

// writeJSON 以指定状态码写出 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeErr 以指定状态码写出 {"error": msg}。
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// auth 中间件：X-Token → store.Agent，失败一律 401（不区分缺失/无效，
// 避免向攻击者泄露 token 空间的有效性信息）。
func (s *Server) auth(next func(w http.ResponseWriter, r *http.Request, ag store.Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Token")
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "缺少 X-Token")
			return
		}
		ag, ok, err := s.St.AgentByToken(tok)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询 token 失败")
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "无效 token")
			return
		}
		next(w, r, ag)
	}
}

// decodeBody 限制 body 上限 1MB 并解析 JSON，防超长输入打爆内存。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	ag, err := s.St.CreateAgent(body.Name)
	if err != nil {
		// 唯一索引冲突（重名）→ 409；其余写库失败 → 500
		writeErr(w, http.StatusConflict, "agent 名已存在")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": strconv.FormatInt(ag.ID, 10), "name": ag.Name, "token": ag.Token,
	})
}

func (s *Server) handleCreateMatch(w http.ResponseWriter, r *http.Request, ag store.Agent) {
	var body struct {
		TaskID string `json:"task_id"`
		AgentA string `json:"agent_a"`
		AgentB string `json:"agent_b"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.TaskID == "" || body.AgentA == "" || body.AgentB == "" {
		writeErr(w, http.StatusBadRequest, "task_id/agent_a/agent_b 均不能为空")
		return
	}
	// 对抗性：task_id 用于路径拼接，必须防目录穿越
	if body.TaskID == ".." || strings.ContainsAny(body.TaskID, "/\\") || strings.Contains(body.TaskID, "..") {
		writeErr(w, http.StatusBadRequest, "task_id 含非法字符")
		return
	}
	if _, err := os.Stat(filepath.Join(s.TasksDir, body.TaskID, "task.json")); err != nil {
		writeErr(w, http.StatusBadRequest, "任务目录不存在: "+body.TaskID)
		return
	}
	a, ok, err := s.St.AgentByName(body.AgentA)
	if err != nil || !ok {
		writeErr(w, http.StatusBadRequest, "agent_a 不存在: "+body.AgentA)
		return
	}
	b, ok, err := s.St.AgentByName(body.AgentB)
	if err != nil || !ok {
		writeErr(w, http.StatusBadRequest, "agent_b 不存在: "+body.AgentB)
		return
	}
	id, err := s.St.CreateMatch(body.TaskID, a.ID, b.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建对局失败")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"match_id":  strconv.FormatInt(id, 10),
		"judge_key": string(s.JudgeKey),
	})
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, ag store.Agent) {
	matchID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "match id 非法")
		return
	}
	_, aID, bID, _, _, err := s.St.MatchByID(matchID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "对局不存在")
		return
	}
	// 上报方必须是对局双方之一
	if ag.ID != aID && ag.ID != bID {
		writeErr(w, http.StatusForbidden, "当前 token 不属于该对局双方")
		return
	}
	var body struct {
		Side           string `json:"side"`
		Passed         int    `json:"passed"`
		Total          int    `json:"total"`
		WallMS         int64  `json:"wall_ms"`
		DiffHash       string `json:"diff_hash"`
		EventsGZBase64 string `json:"events_gz_base64"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Side != "a" && body.Side != "b" {
		writeErr(w, http.StatusBadRequest, `side 必须为 "a" 或 "b"`)
		return
	}
	// M1：非法 base64 直接置空忽略（事件流是辅助数据，不阻断结算）
	eventsGZ, err := base64.StdEncoding.DecodeString(body.EventsGZBase64)
	if err != nil {
		eventsGZ = nil
	}
	done, err := s.St.AddResult(matchID, body.Side, store.Result{
		Passed: body.Passed, Total: body.Total, WallMS: body.WallMS,
		DiffHash: body.DiffHash, EventsGZ: eventsGZ,
	})
	if err != nil {
		// 同侧重复提交被 results 主键拒绝 → 409；其余 → 500
		writeErr(w, http.StatusConflict, "结果写入失败（同侧可能已提交过）: "+err.Error())
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting"})
		return
	}
	// 已结算：回读 winner 与双方最新 rating
	_, _, _, _, winner, err := s.St.MatchByID(matchID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "结算后回读对局失败")
		return
	}
	agA, err := s.St.AgentByID(aID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("读 agent %d 失败", aID))
		return
	}
	agB, err := s.St.AgentByID(bID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("读 agent %d 失败", bID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "done", "winner": winner,
		"rating_a": agA.Rating, "rating_b": agB.Rating,
	})
}

func (s *Server) handleLadder(w http.ResponseWriter, r *http.Request) {
	agents, err := s.St.Ladder()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读天梯失败")
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
	rows := make([]row, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, row{
			Name: a.Name, Rating: a.Rating,
			Games: a.Games, Wins: a.Wins, Losses: a.Losses, Ties: a.Ties,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ladder": rows})
}

func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	// 对抗性：防目录穿越
	if taskID == "" || taskID == "." || taskID == ".." ||
		strings.ContainsAny(taskID, "/\\") || strings.Contains(taskID, "..") {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	root := filepath.Join(s.TasksDir, taskID)
	if _, err := os.Stat(filepath.Join(root, "task.json")); err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+taskID+`.zip"`)
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	})
	if err != nil {
		// 头已发出，无法改状态码；只能中断连接侧的响应
		http.Error(w, "打包失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	zw.Close()
}
