package mcp_test

import (
	"testing"

	wendymcp "github.com/wendylabsinc/wendy/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func TestNew_NotNil(t *testing.T) {
	srv := wendymcp.New(&config.Config{}, nil)
	if srv == nil {
		t.Fatal("New returned nil")
	}
}

func TestGetConn_NilBeforeConnect(t *testing.T) {
	srv := wendymcp.New(&config.Config{}, nil)
	if srv.GetConn() != nil {
		t.Fatal("expected nil connection before connect")
	}
}
