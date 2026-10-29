package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func callWithMeta(t *testing.T, srv *server.MCPServer, name string, meta map[string]any) mcpgo.CallToolResult {
	t.Helper()
	encodedMeta, _ := json.Marshal(meta)
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{},"_meta":%s}}`, name, encodedMeta)
	encoded, _ := json.Marshal(srv.HandleMessage(context.Background(), []byte(msg)))
	var reply struct{ Result mcpgo.CallToolResult }
	if err := json.Unmarshal(encoded, &reply); err != nil {
		t.Fatal(err)
	}
	return reply.Result
}

func TestReadOnlyCallRunsOnlyReadOnlyToolsWithAReadOnlyContext(t *testing.T) {
	srv, err := New(&config.Config{}, nil).newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	ran := map[string]bool{}
	probe := func(name string) server.ToolHandlerFunc {
		return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			ran[name] = grpcclient.IsReadOnly(ctx)
			return mcpgo.NewToolResultText("ok"), nil
		}
	}
	srv.AddTool(mcpgo.NewTool("probe_read", readOnly()...), probe("probe_read"))
	srv.AddTool(mcpgo.NewTool("probe_write", mutating()...), probe("probe_write"))
	readOnlyMeta := map[string]any{readOnlyCallMeta: true}

	if result := callWithMeta(t, srv, "probe_read", readOnlyMeta); result.IsError || !ran["probe_read"] {
		t.Fatalf("read-only tool in a read-only call: result %+v, read-only context %v", result, ran["probe_read"])
	}
	if result := callWithMeta(t, srv, "probe_write", readOnlyMeta); !deniedByBinding(result) {
		t.Fatalf("mutating tool in a read-only call: %+v; want DENIED", result)
	}
	if _, called := ran["probe_write"]; called {
		t.Fatal("a read-only call ran a mutating tool")
	}

	// Without the mark, both run with an ordinary context.
	for _, name := range []string{"probe_read", "probe_write"} {
		if result := callWithMeta(t, srv, name, map[string]any{}); result.IsError || ran[name] {
			t.Fatalf("%s without the mark: result %+v, read-only context %v", name, result, ran[name])
		}
	}
}

// wendy chat sends the mark under its own constant; the two must agree.
func TestReadOnlyCallMetaMatchesChat(t *testing.T) {
	if readOnlyCallMeta != chat.ReadOnlyCallMeta {
		t.Fatalf("mcp reads %q, chat sends %q", readOnlyCallMeta, chat.ReadOnlyCallMeta)
	}
}

// Tools reach the agent through grpcclient's guarded clients, so a call wendy
// chat marks read-only cannot change the device. An AgentConnection's raw Conn
// bypasses that guard; build clients on conn.Client() instead.
func TestToolsReachTheAgentOnlyThroughTheReadOnlyGuard(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		packages := map[string]bool{}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			local := filepath.Base(path)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			packages[local] = true
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Conn" {
				return true
			}
			// net.Conn, tls.Conn and the like are types, not the agent channel.
			if ident, ok := selector.X.(*ast.Ident); ok && packages[ident.Name] {
				return true
			}
			t.Errorf("%s: .Conn reaches the agent past the read-only guard; use .Client()", fset.Position(selector.Pos()))
			return true
		})
	}
}
