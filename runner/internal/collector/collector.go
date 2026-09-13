// Package collector 把 adapter 的 RawEvent 加工成带 hash 链的 protocol.Event。
package collector

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"time"

	"agentbattle/protocol"
	"agentbattle/runner/internal/adapter"
)

// RawEvent 别名避免调用方 import 两个包。
type RawEvent = adapter.RawEvent

type Collector struct {
	mu     sync.Mutex
	seq    int
	events []protocol.Event
}

func New() *Collector { return &Collector{} }

// Add 记录一条事件：打序号、取当前时间、续 hash 链。并发安全。
func (c *Collector) Add(raw RawEvent) protocol.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := protocol.Event{
		Seq:        c.seq,
		TS:         time.Now().UnixMilli(),
		Type:       raw.Type,
		Tool:       raw.Tool,
		ArgsHash:   raw.ArgsHash,
		DurationMS: raw.DurationMS,
		Tokens:     raw.Tokens,
		Note:       raw.Note,
	}
	prev := protocol.GenesisHash
	if c.seq > 0 {
		prev = c.events[c.seq-1].Hash
	}
	e.PrevHash = prev
	e = protocol.SealOne(e)
	c.events = append(c.events, e)
	c.seq++
	return e
}

func (c *Collector) Events() []protocol.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.Event, len(c.events))
	copy(out, c.events)
	return out
}

// FlushNDJSON 把全部事件以 NDJSON 写入 w（一行一个事件）。
func (c *Collector) FlushNDJSON(w io.Writer) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, e := range c.Events() {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return bw.Flush()
}
