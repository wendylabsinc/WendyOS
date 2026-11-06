package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRunLogResourcePreservesEvidenceAndBoundsRetention(t *testing.T) {
	s := New(&config.Config{}, nil)
	first := s.saveRunLog(commandTarget{Device: "vm:test"}, "first build", false)
	var last string
	for i := 0; i < 8; i++ {
		last = s.saveRunLog(commandTarget{Device: "vm:test"}, fmt.Sprint(i), i == 7)
	}
	read := func(uri string) ([]mcpgo.ResourceContents, error) {
		req := mcpgo.ReadResourceRequest{}
		req.Params.URI = uri
		return s.handleRunLogResource(context.Background(), req)
	}
	if _, err := read(first); err == nil {
		t.Fatal("expired log remained in the bounded cache")
	}
	contents, err := read(last)
	if err != nil || len(contents) != 1 {
		t.Fatalf("read log: %v %v", contents, err)
	}
	var record runLogRecord
	if err := json.Unmarshal([]byte(contents[0].(mcpgo.TextResourceContents).Text), &record); err != nil {
		t.Fatal(err)
	}
	if record.Target.Device != "vm:test" || record.Output != "7" || !record.Truncated {
		t.Fatalf("log evidence changed: %+v", record)
	}
	s.setBindingPolicy(&BindingPolicy{AllowRead: true})
	if _, err := read(last); err == nil {
		t.Fatal("read-only grant exposed deployment logs without run access")
	}
}
