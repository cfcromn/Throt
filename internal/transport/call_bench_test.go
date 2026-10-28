package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// BenchmarkCallStatusSized is one client round trip over the Unix socket
// with a status-sized result (about 30 KiB, a machine with 50 agents), as
// the TUI makes every second.
func BenchmarkCallStatusSized(b *testing.B) {
	type agent struct {
		Agent string  `json:"agent"`
		PID   int     `json:"pid"`
		CPU   float64 `json:"cpu"`
		RSS   uint64  `json:"rss"`
		Note  string  `json:"note"`
	}
	result := make([]agent, 50)
	for i := range result {
		result[i] = agent{Agent: "claude-1", PID: 1000 + i, CPU: 0.02, RSS: 150 << 20, Note: strings.Repeat("x", 500)}
	}
	sock := startServer(b, map[string]Handler{
		"status": func(context.Context, json.RawMessage) (any, error) { return result, nil },
	})
	var got []agent
	b.ReportAllocs()
	for b.Loop() {
		if err := Call(context.Background(), sock, "status", nil, &got); err != nil {
			b.Fatal(err)
		}
	}
}
