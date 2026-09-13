// recompute_test.go —— 与 store 的集成测试。
package profile

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"agentbattle/protocol"
	"agentbattle/platform/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// gzEvents 把事件流压成 events_gz 形态（与 runner 侧 GzipEvents 同构）。
func gzEvents(t *testing.T, events []protocol.Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sameProfileExceptTime 比较两份画像 JSON 除 UpdatedAt 外是否一致
//（幂等断言不能依赖重算时刻的墙钟）。
func sameProfileExceptTime(t *testing.T, a, b string) bool {
	t.Helper()
	var pa, pb Profile
	if err := json.Unmarshal([]byte(a), &pa); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &pb); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	pa.UpdatedAt, pb.UpdatedAt = 0, 0
	return reflect.DeepEqual(pa, pb)
}

// TestRecomputeIdempotentAndIsolated 跑通"读窗口 → 聚合 → 落库"，两次重算
// 的维度分完全一致（除 updated_at 外一致），且 task_type 互不污染。
func TestRecomputeIdempotentAndIsolated(t *testing.T) {
	s := newStore(t)
	a, _ := s.CreateAgent("ra")
	b, _ := s.CreateAgent("rb")
	events := []protocol.Event{{Type: protocol.EventToolCall, Tokens: 10}, {Type: protocol.EventFileEdit}}

	mk := func(taskType string, passed int) int64 {
		t.Helper()
		mid, err := s.CreateMatch("tk", taskType, a.ID, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		for side, p := range map[string]int{"a": passed, "b": 0} {
			gz := gzEvents(t, events)
			if p == 0 && side == "b" {
				gz = nil // B 侧事件流缺失：降级路径
			}
			if _, err := s.AddResult(mid, side, store.Result{Passed: p, Total: 2, EventsGZ: gz}); err != nil {
				t.Fatal(err)
			}
		}
		return mid
	}
	mk("debug", 2)
	mk("debug", 2)
	mk("general", 2)
	mk("general", 2)

	if err := Recompute(s, a.ID, "debug"); err != nil {
		t.Fatal(err)
	}
	ps1, err := s.ProfilesByAgent("ra")
	if err != nil || len(ps1) != 1 {
		t.Fatalf("debug 池重算后应 1 条画像: %v %v", ps1, err)
	}
	first := ps1[0].ProfileJSON

	if err := Recompute(s, a.ID, "debug"); err != nil {
		t.Fatal(err)
	}
	ps2, _ := s.ProfilesByAgent("ra")
	if !sameProfileExceptTime(t, first, ps2[0].ProfileJSON) {
		t.Fatalf("同数据两次重算应幂等（除 updated_at 外一致）:\n%s\n%s", first, ps2[0].ProfileJSON)
	}

	// general 是独立池：重算后 debug 画像不受影响
	if err := Recompute(s, a.ID, "general"); err != nil {
		t.Fatal(err)
	}
	ps3, _ := s.ProfilesByAgent("ra")
	if len(ps3) != 2 {
		t.Fatalf("task_type 间应互相隔离: %d", len(ps3))
	}
	for _, p := range ps3 {
		if p.TaskType == "debug" && !sameProfileExceptTime(t, first, p.ProfileJSON) {
			t.Fatal("debug 画像不应被 general 重算波及")
		}
	}
	// 画像可反序列化回 Profile 且样本量正确
	for _, p := range ps3 {
		var doc Profile
		if err := json.Unmarshal([]byte(p.ProfileJSON), &doc); err != nil {
			t.Fatalf("profile_json 应为合法 Profile JSON: %v", err)
		}
		if doc.SampleSize != 2 || len(doc.Dims) != 6 {
			t.Fatalf("样本与维度数错误: %+v", doc)
		}
	}
}

// TestDecodeEventsDegenerate 覆盖 decodeEvents 的降级契约：正常 EOF 返回
// 事件；空输入/坏 gzip 头/中段截断一律整流丢弃返回 nil，不喂残缺数据进画像。
func TestDecodeEventsDegenerate(t *testing.T) {
	good := gzEvents(t, []protocol.Event{{Type: protocol.EventToolCall, Tokens: 5}})

	// a) 正常流往返
	evs := decodeEvents(good)
	if len(evs) != 1 || evs[0].Type != protocol.EventToolCall {
		t.Fatalf("正常流应解出 1 条 ToolCall 事件: %+v", evs)
	}

	// b) 空输入与坏 gzip 头
	if got := decodeEvents(nil); got != nil {
		t.Fatalf("空输入应返回 nil: %+v", got)
	}
	if got := decodeEvents([]byte{0x00}); got != nil {
		t.Fatalf("坏 gzip 头应返回 nil: %+v", got)
	}

	// c) 中段截断（合法 gz 砍掉后半段）：必须整流降级返回 nil
	if got := decodeEvents(good[:len(good)/2]); got != nil {
		t.Fatalf("中段截断应整流返回 nil，不应带残缺数据: %+v", got)
	}
}

// TestRecomputeNoMatches 无任何对局时不落画像（空窗口不产生垃圾行）。
func TestRecomputeNoMatches(t *testing.T) {
	s := newStore(t)
	a, _ := s.CreateAgent("empty")
	if err := Recompute(s, a.ID, "general"); err != nil {
		t.Fatalf("空窗口重算不应报错: %v", err)
	}
	ps, _ := s.ProfilesByAgent("empty")
	if len(ps) != 0 {
		t.Fatalf("空窗口不应落库: %d", len(ps))
	}
}
