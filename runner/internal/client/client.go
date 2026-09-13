// Package client 是 Runner 对接平台的 HTTP 客户端（REST，X-Token 认证），
// 并提供任务包 zip 的安全解包（防 zip slip）。
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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentbattle/protocol"
)

// Client 指向平台 API 根地址。
type Client struct {
	Base string
	HTTP *http.Client
}

// New 创建客户端，base 会去掉尾部 "/"，HTTP 客户端带 60s 超时。
func New(base string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/"),
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

// do 发起请求：body 非 nil 时 JSON 编码；token 非空时带 X-Token 头；
// 非 2xx 读 body（上限 1MB）拼进错误；2xx 且 out 非 nil 时 Decode 响应。
func (c *Client) do(method, path, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequest(method, c.Base+path, rdr)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("请求平台失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		bs, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("平台返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(bs)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("解析平台响应失败: %w", err)
		}
	}
	return nil
}

// registerResp 是注册响应。
type registerResp struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// Register 注册 agent，返回平台分配的 id 与 token。
func (c *Client) Register(name string) (id int64, token string, err error) {
	var out registerResp
	if err := c.do(http.MethodPost, "/api/agents", "", map[string]string{"name": name}, &out); err != nil {
		return 0, "", err
	}
	return out.ID, out.Token, nil
}

// createMatchResp 是创建对局响应。
type createMatchResp struct {
	MatchID  int64  `json:"match_id"`
	JudgeKey string `json:"judge_key"`
}

// CreateMatch 用 token 创建对局，返回对局 ID 与裁判密钥。
func (c *Client) CreateMatch(token, taskID, agentA, agentB string) (matchID int64, judgeKey string, err error) {
	var out createMatchResp
	body := map[string]string{"task_id": taskID, "agent_a": agentA, "agent_b": agentB}
	if err := c.do(http.MethodPost, "/api/matches", token, body, &out); err != nil {
		return 0, "", err
	}
	return out.MatchID, out.JudgeKey, nil
}

// ResultIn 上报给平台的一侧结果。
type ResultIn struct {
	Side          string
	Passed, Total int
	WallMS        int64
	DiffHash      string
	EventsGZ      []byte // 可空；非空时 base64 编码进 events_gz_base64
}

// resultReq 是上报请求体（带 json tag 的内部形态）。
type resultReq struct {
	Side           string `json:"side"`
	Passed         int    `json:"passed"`
	Total          int    `json:"total"`
	WallMS         int64  `json:"wall_ms"`
	DiffHash       string `json:"diff_hash,omitempty"`
	EventsGZBase64 string `json:"events_gz_base64,omitempty"`
}

// Settle 是上报后的结算响应。
type Settle struct {
	Status  string  `json:"status"`
	Winner  string  `json:"winner"`
	RatingA float64 `json:"rating_a"`
	RatingB float64 `json:"rating_b"`
}

// UploadResult 上报对局结果并取回结算。
func (c *Client) UploadResult(token string, matchID int64, r ResultIn) (Settle, error) {
	req := resultReq{
		Side:     r.Side,
		Passed:   r.Passed,
		Total:    r.Total,
		WallMS:   r.WallMS,
		DiffHash: r.DiffHash,
	}
	if len(r.EventsGZ) > 0 {
		req.EventsGZBase64 = base64.StdEncoding.EncodeToString(r.EventsGZ)
	}
	var out Settle
	path := fmt.Sprintf("/api/matches/%d/results", matchID)
	if err := c.do(http.MethodPost, path, token, req, &out); err != nil {
		return Settle{}, err
	}
	return out, nil
}

// LadderRow 是天梯上的一行。
type LadderRow struct {
	Name   string  `json:"name"`
	Rating float64 `json:"rating"`
	Games  int     `json:"games"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	Ties   int     `json:"ties"`
}

// ladderResp 是天梯响应。
type ladderResp struct {
	Ladder []LadderRow `json:"ladder"`
}

// Ladder 拉取天梯。空列表返回 nil 切片，无错。
func (c *Client) Ladder() ([]LadderRow, error) {
	var out ladderResp
	if err := c.do(http.MethodGet, "/api/ladder", "", nil, &out); err != nil {
		return nil, err
	}
	return out.Ladder, nil
}

// FetchBundle 拉取任务包 zip 字节，响应上限 256MB。
func (c *Client) FetchBundle(taskID string) ([]byte, error) {
	path := "/api/tasks/" + url.PathEscape(taskID) + "/bundle"
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求平台失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bs, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("拉取任务包 %s 返回 %d: %s", taskID, resp.StatusCode, strings.TrimSpace(string(bs)))
	}
	bs, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("读取任务包失败: %w", err)
	}
	if int64(len(bs)) == 256<<20 {
		// 响应被 LimitReader 截断到上限，说明实际包体达到/超过 256MB，
		// 不能把截断的 zip 静默返回（下游解包会报出误导性的损坏错误）
		return nil, fmt.Errorf("任务包超过 256MB 上限")
	}
	return bs, nil
}

// zip 名段中的 ".." 才是穿越；"a..b.txt" 这类合法名不受影响。
func isTraversalName(name string) bool {
	name = filepath.ToSlash(name)
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return true
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// ExtractBundle 解压任务包到 destDir，拒绝路径穿越（zip slip）：
// 条目名为绝对路径或任一路径段为 ".." 时报错；解压目标经
// filepath.Abs + HasPrefix 双重校验；单文件上限 64MB（防 zip 炸弹）。
func ExtractBundle(zipBytes []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("读取 zip 失败: %w", err)
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("解析目标目录失败: %w", err)
	}
	for _, f := range zr.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if isTraversalName(f.Name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("zip 条目含非法路径: %q", f.Name)
		}
		target := filepath.Join(absDest, name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("解析条目路径失败: %w", err)
		}
		if absTarget != absDest && !strings.HasPrefix(absTarget, absDest+string(filepath.Separator)) {
			return fmt.Errorf("zip 条目越界目标目录: %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(absTarget, 0o755); err != nil {
				return fmt.Errorf("创建目录 %s 失败: %w", absTarget, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
			return fmt.Errorf("创建父目录 %s 失败: %w", filepath.Dir(absTarget), err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("打开条目 %q 失败: %w", f.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, (64<<20)+1))
		rc.Close()
		if err != nil {
			return fmt.Errorf("读取条目 %q 失败: %w", f.Name, err)
		}
		if int64(len(data)) > 64<<20 {
			return fmt.Errorf("条目 %q 超过单文件 64MB 上限", f.Name)
		}
		if err := os.WriteFile(absTarget, data, 0o644); err != nil {
			return fmt.Errorf("写出条目 %q 失败: %w", f.Name, err)
		}
	}
	return nil
}

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

// ReviewSideOut 复盘单侧概要。
type ReviewSideOut struct {
	Agent     string `json:"agent"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	WallMS    int64  `json:"wall_ms"`
	HasEvents bool   `json:"has_events"`
}

// ReviewTlEvent 复盘时间线事件（runner 不能导入 platform/internal/review
// ——internal 边界，本地定义最小解析形态，与平台侧 JSON 契约对齐）。
type ReviewTlEvent struct {
	Seq        int    `json:"seq"`
	TS         int64  `json:"ts"`
	Type       string `json:"type"`
	Tool       string `json:"tool"`
	Path       string `json:"path"`
	DurationMS int64  `json:"duration_ms"`
	Tokens     int    `json:"tokens"`
}

// ReviewMark 关键节点标注（M2 仅 first_error）。
type ReviewMark struct {
	Kind string `json:"kind"`
	Seq  int    `json:"seq"`
}

// ReviewPair 一项对比指标的 A/B 值。
type ReviewPair struct {
	A float64 `json:"a"`
	B float64 `json:"b"`
}

// ReviewCompare 双侧行为对比。
type ReviewCompare struct {
	PassRatio ReviewPair `json:"pass_ratio"`
	ToolCalls ReviewPair `json:"tool_calls"`
	Errors    ReviewPair `json:"errors"`
	Edits     ReviewPair `json:"edits"`
	Tokens    ReviewPair `json:"tokens"`
	WallMS    ReviewPair `json:"wall_ms"`
}

// ReviewReport 对局复盘报告。
type ReviewReport struct {
	MatchID  int64                      `json:"match_id"`
	TaskID   string                     `json:"task_id"`
	TaskType string                     `json:"task_type"`
	Status   string                     `json:"status"`
	Winner   string                     `json:"winner"`
	Sides    map[string]ReviewSideOut   `json:"sides"`
	Timeline map[string][]ReviewTlEvent `json:"timeline"`
	Marks    map[string][]ReviewMark    `json:"marks"`
	Compare  ReviewCompare              `json:"compare"`
}

// Review 拉取指定对局的复盘报告（公开路由，无需 token）。
func (c *Client) Review(matchID int64) (ReviewReport, error) {
	var out ReviewReport
	path := fmt.Sprintf("/api/matches/%d/review", matchID)
	if err := c.do(http.MethodGet, path, "", nil, &out); err != nil {
		return ReviewReport{}, err
	}
	return out, nil
}

// GzipEvents 把事件流序列化为 NDJSON 并 gzip 压缩（上报用；
// 事件仅含路径/操作类型，无文件内容明文——隐私红线见项目 CLAUDE.md）。
func GzipEvents(events []protocol.Event) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			gw.Close()
			return nil, fmt.Errorf("编码事件失败: %w", err)
		}
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("关闭 gzip writer 失败: %w", err)
	}
	return buf.Bytes(), nil
}
