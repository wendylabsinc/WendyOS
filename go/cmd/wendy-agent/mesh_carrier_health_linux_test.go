//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

func TestCarrierStartupFailureAndConfigurationRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	if err := os.WriteFile(path, []byte(`{"nan":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	sharing := &meshSharingManager{}
	// Missing enrollment cert fails before any kernel/network mutation.
	for range 2 {
		err := runConfiguredMeshCarriers(context.Background(), dir, localmesh.TCPIdentity{Org: 64, Asset: 536}, zap.NewNop(), nil, sharing, nil)
		if err == nil {
			t.Fatal("invalid identity was accepted")
		}
		state := sharing.Status()
		if state.State != "failed" || !strings.Contains(state.Detail, "NAN: failed") {
			t.Fatalf("startup failure masked: %+v", state)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := runConfiguredMeshCarriers(context.Background(), dir, localmesh.TCPIdentity{Org: 64, Asset: 536}, zap.NewNop(), nil, sharing, nil); err != nil {
		t.Fatal(err)
	}
	if state := sharing.Status(); state.Available || state.State != "" {
		t.Fatalf("removed config retained failure: %+v", state)
	}
}
