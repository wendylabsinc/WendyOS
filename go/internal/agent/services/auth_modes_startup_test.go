package services

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A29 passive startup-state fixture: an unusable dormant local store cannot
// replace enrollment. This does not simulate supervisor fallback or a live fault.
func TestAuthModesA29EnrolledPrecedence(t *testing.T) {
	keys := trustedkeys.Unusable(filepath.Join(t.TempDir(), trustedkeys.FileName))
	restarts := 0
	s := NewAccessService(zap.NewNop(), keys, func() bool { return true }, func() { restarts++ })
	ctx := context.Background()
	state, err := s.GetAccess(ctx, &pb.GetAccessRequest{})
	if err != nil || state.GetShape() != pb.AccessShape_ACCESS_SHAPE_ENROLLED || state.GetCallerRole() != pb.TrustedKeyRole_TRUSTED_KEY_ROLE_UNSPECIFIED {
		t.Fatalf("unusable dormant state replaced enrollment: %v %v", state, err)
	}
	for _, call := range []func() error{
		func() error { _, e := s.CloseDevice(ctx, &pb.CloseDeviceRequest{}); return e },
		func() error { _, e := s.OpenDevice(ctx, &pb.OpenDeviceRequest{}); return e },
		func() error { _, e := s.SetKeylessApps(ctx, &pb.SetKeylessAppsRequest{Enabled: true}); return e },
		func() error { _, e := s.AddTrustedKey(ctx, &pb.AddTrustedKeyRequest{}); return e },
		func() error { _, e := s.RemoveTrustedKey(ctx, &pb.RemoveTrustedKeyRequest{}); return e },
		func() error { _, e := s.ListTrustedKeys(ctx, &pb.ListTrustedKeysRequest{}); return e },
	} {
		if err := call(); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("Enrolled gate with unusable store: %v", err)
		}
	}
	if restarts != 0 || len(keys.List()) != 0 || !keys.Closed() || keys.KeylessApps() {
		t.Fatal("Enrolled refusal changed unusable dormant state")
	}
}
