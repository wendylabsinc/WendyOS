package mcp

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// TestExportGo2EvalCatalog exports the real schemas without starting MCP,
// discovering devices, or executing handlers. The chat eval supplies its own
// in-memory Executor; none of these registered handlers leave this test.
func TestExportGo2EvalCatalog(t *testing.T) {
	path := os.Getenv("WENDY_GO2_EVAL_CATALOG")
	if path == "" {
		t.Skip("set WENDY_GO2_EVAL_CATALOG to export schemas for the simulated chat eval")
	}
	s := New(&config.Config{}, nil)
	srv := server.NewMCPServer("go2-eval-catalog", "1")
	s.registerStatusTools(srv)
	s.registerDeviceTools(srv)
	s.registerRobotTools(srv)
	s.registerRobotCameraTools(srv)
	s.registerRobotFindTools(srv)
	var catalog struct {
		Tools []mcpgo.Tool `json:"tools"`
	}
	for _, entry := range srv.ListTools() {
		catalog.Tools = append(catalog.Tools, entry.Tool)
	}
	sort.Slice(catalog.Tools, func(i, j int) bool { return catalog.Tools[i].Name < catalog.Tools[j].Name })
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
