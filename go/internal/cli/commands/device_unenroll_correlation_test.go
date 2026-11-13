package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/metadata"
)

func TestV2UnenrollCloudContextPreservesOnlyValidatedFlow(t *testing.T) {
	input := metadata.Pairs("x-correlation-id", "", "x-correlation-id", "existing.flow-123", "agent-only", "not-for-cloud")
	ctx := metadata.NewOutgoingContext(context.Background(), input)
	rpcctx, err := v2UnenrollCloudContext(ctx, &config.AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(rpcctx)
	if got := md.Get("x-correlation-id"); len(got) != 1 || got[0] != "existing.flow-123" {
		t.Fatalf("operation id replaced: %v", got)
	}
	if got := md.Get("agent-only"); len(got) != 0 {
		t.Fatalf("unrelated metadata leaked to Cloud: %v", got)
	}
	if len(input.Get("x-correlation-id")) != 2 {
		t.Fatal("caller metadata mutated")
	}
	for range 3 {
		id, err := cloudrequest.CorrelationID(rpcctx)
		if err != nil || id != "existing.flow-123" {
			t.Fatalf("later Cloud phase replaced id: %s, %v", id, err)
		}
	}
}

func TestV2UnenrollCloudContextStartsOneNewFlow(t *testing.T) {
	rpcctx, err := v2UnenrollCloudContext(context.Background(), &config.AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(rpcctx)
	ids := md.Get("x-correlation-id")
	if len(ids) != 1 {
		t.Fatalf("expected one origin id: %v", ids)
	}
	if _, err := uuid.Parse(ids[0]); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		id, err := cloudrequest.CorrelationID(rpcctx)
		if err != nil || id != ids[0] {
			t.Fatalf("later Cloud phase replaced id: %s, %v", id, err)
		}
	}
}

func TestV2UnenrollCloudContextRejectsUnsafeFlow(t *testing.T) {
	for _, id := range []string{"space value", "line\nbreak", "ünicode", strings.Repeat("a", 65)} {
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-correlation-id", id))
		if _, err := v2UnenrollCloudContext(ctx, &config.AuthConfig{}); err == nil {
			t.Fatalf("unsafe flow accepted: %q", id)
		}
	}
}
