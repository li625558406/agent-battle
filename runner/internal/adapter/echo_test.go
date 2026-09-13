package adapter

import (
	"context"
	"testing"
	"time"
)

func TestEchoSolvesTask(t *testing.T) {
	cwd := t.TempDir()
	a := Echo{FixContent: "fixed"}
	ch := make(chan RawEvent, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := a.Launch(ctx, cwd, "fix it", nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	close(ch)
	types := map[string]int{}
	for e := range ch {
		types[e.Type]++
	}
	if types["tool_call"] == 0 || types["result"] == 0 {
		t.Fatalf("echo should emit tool_call and result, got %v", types)
	}
}
