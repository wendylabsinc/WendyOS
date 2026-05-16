package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func TestNew_NotNil(t *testing.T) {
	srv := New(&config.Config{}, nil)
	if srv == nil {
		t.Fatal("New returned nil")
	}
}

func TestGetConn_NilBeforeConnect(t *testing.T) {
	srv := New(&config.Config{}, nil)
	if srv.GetConn() != nil {
		t.Fatal("expected nil connection before connect")
	}
}

func TestGuideResource_ReturnsText(t *testing.T) {
	srv := New(&config.Config{}, nil)
	contents, err := srv.handleGuideResource(context.Background(), mcpgo.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(contents))
	}
	tc, ok := contents[0].(mcpgo.TextResourceContents)
	if !ok {
		t.Fatalf("expected TextResourceContents, got %T", contents[0])
	}
	if tc.URI != "wendy://guide" {
		t.Errorf("expected URI wendy://guide, got %q", tc.URI)
	}
	if tc.MIMEType != "text/plain" {
		t.Errorf("expected MIME text/plain, got %q", tc.MIMEType)
	}
	if len(tc.Text) < 100 {
		t.Errorf("expected guide text to be at least 100 chars, got %d", len(tc.Text))
	}
}
