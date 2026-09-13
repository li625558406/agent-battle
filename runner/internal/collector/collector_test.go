package collector

import (
	"bytes"
	"encoding/json"
	"testing"

	"agentbattle/protocol"
)

func TestAddSealsChain(t *testing.T) {
	c := New()
	e1 := c.Add(RawEvent{Type: protocol.EventToolCall, Tool: "Bash"})
	e2 := c.Add(RawEvent{Type: protocol.EventResult, Tokens: 10})
	if e1.Seq != 0 || e2.Seq != 1 {
		t.Fatalf("seq broken: %d %d", e1.Seq, e2.Seq)
	}
	if e2.PrevHash != e1.Hash {
		t.Fatal("chain not linked")
	}
	if err := protocol.Chain(c.Events()); err != nil {
		t.Fatalf("chain invalid: %v", err)
	}
}

func TestConcurrentAdd(t *testing.T) {
	c := New()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			for j := 0; j < 25; j++ {
				c.Add(RawEvent{Type: protocol.EventMessage})
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if len(c.Events()) != 100 {
		t.Fatalf("lost events: %d", len(c.Events()))
	}
	if err := protocol.Chain(c.Events()); err != nil {
		t.Fatalf("chain invalid under concurrency: %v", err)
	}
}

func TestFlushNDJSON(t *testing.T) {
	c := New()
	c.Add(RawEvent{Type: protocol.EventMessage})
	var buf bytes.Buffer
	if err := c.FlushNDJSON(&buf); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	var e protocol.Event
	if err := json.Unmarshal(lines[0], &e); err != nil {
		t.Fatal(err)
	}
	if e.Hash == "" {
		t.Fatal("event lost hash in serialization")
	}
}
