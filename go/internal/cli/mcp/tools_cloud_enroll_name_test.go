package mcp

import (
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
)

func TestCloudEnrollToolTakenName(t *testing.T) {
	err := fmt.Errorf("Cloud refused this enrollment before minting anything: %w: rpc error", cloudenroll.ErrDeviceNameTaken)
	r := enrollMintErrResult("brave-dolphin", err)
	m, ok := r.StructuredContent.(map[string]any)
	want := "device name brave-dolphin is already used by another device in this organization; call again with a different name"
	if !ok || m["message"] != want || !r.IsError {
		t.Fatalf("result %+v; want error message %q", r.StructuredContent, want)
	}
}
