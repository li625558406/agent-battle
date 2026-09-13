// Package protocol 定义 AgentBattle 的任务/事件/判分契约。
//
// 事件 hash 的可验证性绑定本 Go 实现：hash 基于 Go encoding/json 的
// canonical 序列化（含 HTML 转义、omitempty、字段声明顺序等 Go 特有行为），
// 跨语言独立复算不受支持；链校验必须使用本包的 Chain / SealOne，
// 不得在其他语言或环境下自行重算 hash 比对。
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// GenesisHash 是链头哨兵值。
//
// hash 的可验证性绑定 Go 实现：基于 Go encoding/json 的 canonical 序列化
// （含 HTML 转义等 Go 特有行为），跨语言独立复算不受支持，验证必须使用本包。
const GenesisHash = "GENESIS"

// Seal 一次性封印完整切片：依次为 events 计算 Seq、PrevHash 与 Hash
// （原地修改，O(n)）。封印后的切片不可再追加——追加的新事件无法接到旧链尾，
// 必须整条重封或改用 SealOne 增量模式。
//
// 增量采集场景（事件逐条产生、可能并发追加）不要用 Seal，
// 应持锁维护当前 seq 与尾 hash，并逐条调用 SealOne
// （并发采集器在后续任务实现）。
func Seal(events []Event) {
	prev := GenesisHash
	for i := range events {
		events[i].Seq = i
		events[i].PrevHash = prev
		events[i].Hash = compute(events[i])
		prev = events[i].Hash
	}
}

// SealOne 只计算并填充 e.Hash（PrevHash/Seq 由调用方先填好）。
//
// 用于增量采集模式：调用方持锁维护 seq 与尾 hash，每产生一条事件就
// 填好 Seq/PrevHash 后调用 SealOne，再更新尾 hash。与一次性封印
// 整个切片的 Seal 相对；两种模式不可混用于同一条链。
func SealOne(e Event) Event {
	e.Hash = compute(e)
	return e
}

// Chain 校验事件链完整性：seq 连续、hash 逐条吻合、prev_hash 逐环衔接。
//
// 空链（nil 或零长度切片）视为合法（返回 nil error）；
// 调用方自行决定空链是否可疑。
func Chain(events []Event) error {
	prev := GenesisHash
	for i, e := range events {
		if e.Seq != i {
			return fmt.Errorf("event %d: seq=%d, want %d（疑似删改/乱序）", i, e.Seq, i)
		}
		if compute(e) != e.Hash {
			return fmt.Errorf("event %d: hash 不匹配（链条断裂）", i)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("event %d: prev_hash 不匹配", i)
		}
		prev = e.Hash
	}
	return nil
}

func compute(e Event) string {
	e.Hash = "" // Hash 本身不入 hash
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // Event 全为可序列化基础类型，不可能失败
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
