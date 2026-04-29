package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDeviceInfoModelPausesAutoRefreshForUnimplementedAgent(t *testing.T) {
	model := newDeviceInfoModel(
		nil,
		context.Background(),
		&agentpb.GetAgentVersionResponse{},
		nil,
		status.Error(codes.Unimplemented, "method GetSystemInfo not implemented"),
		"",
		false,
	)

	if model.autoRefresh {
		t.Fatal("autoRefresh should be disabled for unimplemented agents")
	}
	if cmd := model.Init(); cmd != nil {
		t.Fatal("Init should not schedule periodic ticks when auto-refresh is disabled")
	}
	if !strings.Contains(model.footer(), "auto-refresh paused") {
		t.Fatalf("footer = %q; want auto-refresh paused hint", model.footer())
	}
}
