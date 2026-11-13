package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestEveryToolHasTitleAndHints(t *testing.T) {
	srv, err := New(&config.Config{}, nil).newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tools := srv.ListTools()
	for name, st := range tools {
		a := st.Tool.Annotations
		if st.Tool.Title == "" || a.Title == "" {
			t.Errorf("%s has no title; add it to toolTitles", name)
		}
		if a.ReadOnlyHint == nil || a.DestructiveHint == nil || a.IdempotentHint == nil || a.OpenWorldHint == nil {
			t.Errorf("%s lacks an explicit hint: %+v", name, a)
		}
	}
	for name := range toolTitles {
		if _, ok := tools[name]; !ok {
			t.Errorf("toolTitles has %s, which is not registered", name)
		}
	}
}

func TestManifestVersion(t *testing.T) {
	a, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != b.Version || len(a.Tools) == 0 {
		t.Fatalf("manifest not stable: %s vs %s (%d tools)", a.Version, b.Version, len(a.Tools))
	}
	if err := CheckRelayManifest("0.0.1+" + manifestHash(a.Version)); err != nil {
		t.Errorf("same tool set from another CLI release refused: %v", err)
	}
	for _, v := range []string{"", a.Version + "0", "0.0.1+deadbeefdeadbeef"} {
		if CheckRelayManifest(v) == nil {
			t.Errorf("relay manifest %q accepted", v)
		}
	}
}

func TestRelayManifestScope(t *testing.T) {
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, tool := range m.Tools {
		served[tool.Name] = true
		a := tool.Annotations
		if tool.Title == "" || a.ReadOnlyHint == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
			t.Errorf("%s lacks a title or a hint: %+v", tool.Name, a)
		}
		for param := range tool.InputSchema.Properties {
			if strings.Contains(param, "password") || strings.Contains(param, "token") || strings.Contains(param, "secret") {
				t.Errorf("%s takes credential %s from the chat; add it to relayExcluded", tool.Name, param)
			}
		}
	}
	for _, name := range relayExcluded {
		if served[name] {
			t.Errorf("manifest serves excluded tool %s", name)
		}
	}

	s := New(&config.Config{}, nil)
	s.relay = true
	handler := s.relayManifestMiddleware(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		t.Fatal("an excluded tool reached its handler on the relay channel")
		return nil, nil
	})
	var req mcpgo.CallToolRequest
	req.Params.Name = "wifi_connect"
	if res, _ := handler(context.Background(), req); res == nil || !res.IsError {
		t.Errorf("relay call to wifi_connect not refused: %+v", res)
	}
}
