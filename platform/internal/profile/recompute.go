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
