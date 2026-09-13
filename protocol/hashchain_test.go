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
