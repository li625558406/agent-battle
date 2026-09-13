package protocol

import "testing"

func mkChain(n int) []Event {
	evs := make([]Event, n)
	for i := range evs {
		evs[i] = Event{Type: EventMessage, Note: "e"}
	}
	return evs
}

func TestChainRoundTrip(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	if err := Chain(evs); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
}

func TestChainDetectsTamper(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	evs[1].Tokens = 999
	if err := Chain(evs); err == nil {
		t.Fatal("tampered chain not detected")
	}
}

func TestChainDetectsDeletion(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	broken := []Event{evs[0], evs[2]}
	if err := Chain(broken); err == nil {
		t.Fatal("deleted chain not detected")
	}
}

func TestChainDetectsReorder(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	evs[0], evs[1] = evs[1], evs[0]
	if err := Chain(evs); err == nil {
		t.Fatal("reordered chain not detected")
	}
}

// TestChainDetectsBrokenPrevHash 专门覆盖 prev_hash 校验分支：
// 篡改 evs[1].PrevHash 后用 SealOne 重算其单条 hash，使得
// seq 仍连续、每条 hash 各自自洽，唯独 prev 链接断裂——
// 若 Chain 只查 seq 和单条 hash，此链会被误判为合法。
func TestChainDetectsBrokenPrevHash(t *testing.T) {
	evs := mkChain(3)
	Seal(evs)
	evs[1].PrevHash = GenesisHash
	evs[1] = SealOne(evs[1]) // seq 连续、单条 hash 自洽，仅 prev 链接断了
	if err := Chain(evs); err == nil {
		t.Fatal("broken prev_hash chain not detected")
	}
}

// TestGoldenHash 锁定当前实现的 hash 值。
// golden 值基于 Go encoding/json 的 canonical 序列化（含 HTML 转义、
// omitempty、字段声明顺序等 Go 特有行为）——改动 canonical 序列化
// 即破坏历史审计数据的可复算性，此测试将在改动发生时立即失败。
func TestGoldenHash(t *testing.T) {
	e := SealOne(Event{Seq: 0, TS: 0, Type: EventMessage, Note: "golden", PrevHash: GenesisHash})
	// 由 TestGoldenHash 当前实现实测得出（2026-09-13）。
	const want = "8d3aa712d6e38387c43dabb5b4f8fdee421fac157ca8ff6176fb4d1dcb24e6b0"
	if e.Hash != want {
		t.Fatalf("golden hash changed (canonical serialization was altered?):\n got %s\nwant %s", e.Hash, want)
	}
}
