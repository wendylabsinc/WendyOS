package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const deployedRelayManifest = "2026.10.13-212424+7d3624a60bf68a8c"

func TestResolveRelayManifestKeepsPublishedCatalog(t *testing.T) {
	old, err := ResolveRelayManifest(deployedRelayManifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Tools) != 58 || old.Version != deployedRelayManifest {
		t.Fatalf("unexpected published profile: %s (%d tools)", old.Version, len(old.Tools))
	}
	raw, err := json.Marshal(old.Tools)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if got := hex.EncodeToString(hash[:8]); got != manifestHash(old.Version) {
		t.Fatalf("bundled profile content changed: %s", got)
	}
	// Callers receive their own data; changing it cannot widen later sessions.
	old.Tools[0].Name = "caller-injected"
	resolved, err := ResolveRelayManifest("another-cli+7d3624a60bf68a8c")
	if err != nil || resolved.Tools[0].Name == "caller-injected" {
		t.Fatalf("resolution reused caller-owned definitions: %v", err)
	}
	for _, unknown := range []string{"", "old+deadbeefdeadbeef", "old+../manifests/7d3624a60bf68a8c", "old+7d3624a60bf68a8c0"} {
		if _, err := ResolveRelayManifest(unknown); err == nil {
			t.Errorf("accepted unknown manifest %q", unknown)
		}
	}
}

func TestRelayManifestCompatibilityRequiresAdditiveChanges(t *testing.T) {
	base := ToolManifest{Tools: []mcpgo.Tool{mcpgo.NewTool("example",
		mcpgo.WithString("mode", mcpgo.Required(), mcpgo.Enum("a", "b"), mcpgo.DefaultString("a")),
		mcpgo.WithNumber("limit", mcpgo.Min(1), mcpgo.Max(10)),
		mcpgo.WithObject("options", mcpgo.Properties(map[string]any{"label": map[string]any{"type": "string"}})),
		mcpgo.WithReadOnlyHintAnnotation(true),
	)}}
	for _, tc := range []struct {
		name string
		edit func(*ToolManifest)
		ok   bool
	}{
		{"optional argument", func(m *ToolManifest) { m.Tools[0].InputSchema.Properties["extra"] = map[string]any{"type": "string"} }, true},
		{"optional argument with default", func(m *ToolManifest) {
			m.Tools[0].InputSchema.Properties["force"] = map[string]any{"type": "boolean", "default": true}
		}, false},
		{"enum expansion", func(m *ToolManifest) {
			m.Tools[0].InputSchema.Properties["mode"].(map[string]any)["enum"] = []string{"a", "b", "c"}
		}, true},
		{"new tool", func(m *ToolManifest) { m.Tools = append(m.Tools, mcpgo.NewTool("new_tool")) }, true},
		{"descriptions", func(m *ToolManifest) {
			m.Tools[0].Description = "new explanation"
			m.Tools[0].Title = "New display name"
			m.Tools[0].Annotations.Title = "New display name"
			m.Tools[0].InputSchema.Properties["mode"].(map[string]any)["description"] = "new explanation"
		}, true},
		{"removed tool", func(m *ToolManifest) { m.Tools = nil }, false},
		{"removed argument", func(m *ToolManifest) { delete(m.Tools[0].InputSchema.Properties, "mode") }, false},
		{"required addition", func(m *ToolManifest) {
			m.Tools[0].InputSchema.Properties["extra"] = map[string]any{"type": "string"}
			m.Tools[0].InputSchema.Required = append(m.Tools[0].InputSchema.Required, "extra")
		}, false},
		{"enum narrowing", func(m *ToolManifest) {
			m.Tools[0].InputSchema.Properties["mode"].(map[string]any)["enum"] = []string{"a"}
		}, false},
		{"new enum constraint", func(m *ToolManifest) { m.Tools[0].InputSchema.Properties["limit"].(map[string]any)["enum"] = []int{1} }, false},
		{"type change", func(m *ToolManifest) { m.Tools[0].InputSchema.Properties["mode"].(map[string]any)["type"] = "number" }, false},
		{"range narrowing", func(m *ToolManifest) { m.Tools[0].InputSchema.Properties["limit"].(map[string]any)["maximum"] = 5 }, false},
		{"changed default", func(m *ToolManifest) { m.Tools[0].InputSchema.Properties["mode"].(map[string]any)["default"] = "b" }, false},
		{"nested argument addition", func(m *ToolManifest) {
			m.Tools[0].InputSchema.Properties["options"].(map[string]any)["properties"].(map[string]any)["extra"] = map[string]any{"type": "string"}
		}, false},
		{"mutating hint", func(m *ToolManifest) { value := false; m.Tools[0].Annotations.ReadOnlyHint = &value }, false},
		{"destructive hint", func(m *ToolManifest) { value := false; m.Tools[0].Annotations.DestructiveHint = &value }, false},
		{"idempotent hint", func(m *ToolManifest) { value := true; m.Tools[0].Annotations.IdempotentHint = &value }, false},
		{"open world hint", func(m *ToolManifest) { value := false; m.Tools[0].Annotations.OpenWorldHint = &value }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var current ToolManifest
			if err := json.Unmarshal(raw, &current); err != nil {
				t.Fatal(err)
			}
			tc.edit(&current)
			err := compatibleRelayManifest(base, current)
			if (err == nil) != tc.ok {
				t.Fatalf("compatible = %v, want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestRelayCurrentProfileKeepsNewTools(t *testing.T) {
	manifest, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, nil)
	s.relay = true
	started := false
	s.setBindingPolicy(&BindingPolicy{AllowAll: true, RelayManifest: &manifest, Confirm: func(context.Context, string, map[string]any) bool { return true }})
	s.SetSimulatorBackend(SimulatorBackend{Start: func(_ context.Context, name string) (*SimulatorInfo, error) {
		started = true
		return &SimulatorInfo{Name: name, Device: "vm:" + name}, nil
	}})
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	if got := protocolTools(t, srv); len(got) != len(manifest.Tools) {
		t.Fatalf("current profile listed %d tools, want %d", len(got), len(manifest.Tools))
	}
	if reply := callProtocolTool(t, srv, "simulator_start", map[string]any{"name": "dev"}); reply.IsError || !started {
		t.Fatalf("current profile lost simulator_start: %+v", reply)
	}
	// SDK GetArguments turns non-object JSON into nil; reject it before that
	// conversion can disguise an invalid call as an empty argument object.
	encoded, err := json.Marshal(srv.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wendy_status","arguments":[1]}}`)))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct{ Result mcpgo.CallToolResult }
	if err := json.Unmarshal(encoded, &reply); err != nil || !reply.Result.IsError {
		t.Fatalf("non-object arguments accepted: %s (%v)", encoded, err)
	}
}

func TestRelayPublishedProfileControlsListingAndDispatch(t *testing.T) {
	manifest, err := ResolveRelayManifest(deployedRelayManifest)
	if err != nil {
		t.Fatal(err)
	}
	var expected mcpgo.Tool
	for _, tool := range manifest.Tools {
		if tool.Name == "simulator_create" {
			expected = tool
		}
	}
	// The server trusts the known version, not modified caller-owned tool definitions.
	manifest.Tools = nil
	confirmed, created, started := 0, 0, 0
	s := New(&config.Config{}, nil)
	s.relay = true
	s.setBindingPolicy(&BindingPolicy{AllowAll: true, RelayManifest: manifest, Confirm: func(context.Context, string, map[string]any) bool {
		confirmed++
		return true
	}})
	s.SetSimulatorBackend(SimulatorBackend{
		Create: func(_ context.Context, opts SimulatorCreateOptions) (*SimulatorInfo, error) {
			created++
			return &SimulatorInfo{Name: opts.Name, Device: "vm:" + opts.Name, Profile: opts.Profile, State: "stopped"}, nil
		},
		Start: func(context.Context, string) (*SimulatorInfo, error) { started++; return &SimulatorInfo{}, nil },
	})
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	listed := protocolTools(t, srv)
	if len(listed) != 58 {
		t.Fatalf("listed %d tools, want the 58 published tools", len(listed))
	}
	for _, tool := range listed {
		if tool.Name == "simulator_start" {
			t.Fatal("new tool appeared in the old catalog")
		}
		if tool.Name == "simulator_create" && !reflect.DeepEqual(tool, expected) {
			t.Fatal("listed simulator_create differs from the published definition")
		}
	}
	assertProtocolToolHidden(t, srv, "simulator_start")
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"simulator_create", map[string]any{"name": "dev", "profile": "wendy-lite"}},
		{"simulator_create", map[string]any{"name": "dev", "profile": "wendy-lite-native"}},
		{"simulator_create", map[string]any{"name": "dev", "new_argument": true}},
		{"simulator_create", map[string]any{"profile": "generic"}},
	} {
		if reply := callProtocolTool(t, srv, call.name, call.args); !reply.IsError {
			t.Fatalf("unpublished call accepted: %s %v", call.name, call.args)
		}
	}
	if confirmed != 0 || created != 0 || started != 0 {
		t.Fatalf("unpublished calls reached confirmation or handlers: %d %d %d", confirmed, created, started)
	}
	for _, profile := range []string{"generic", "go2", "g1", "rosmaster-r2"} {
		reply := callProtocolTool(t, srv, "simulator_create", map[string]any{"name": "dev", "profile": profile})
		if reply.IsError {
			t.Fatalf("published profile %s failed: %+v", profile, reply)
		}
	}
	if confirmed != 0 || created != 4 || started != 0 {
		t.Fatalf("published calls with --allow-all prompted or lost create behavior: %d %d %d", confirmed, created, started)
	}
}
