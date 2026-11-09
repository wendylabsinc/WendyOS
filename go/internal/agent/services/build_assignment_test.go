package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/buildassignment"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestBuildHostRequiresMatchingAssignmentAndDurableTargetAdmission(t *testing.T) {
	l := newDeliveryRPCLab(t)
	worker := delivery.NewManager(t.TempDir(), "localhost:5000/", func() (string, string) { return l.f.Builder.PEM, l.f.Chain })
	t.Cleanup(worker.Close)
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{DeliverySource: worker.Get, ResolveTarget: func(string) (DeliveryTarget, error) { return DeliveryTarget{Device: l.f.TargetID}, nil }})
	svc.dialTarget = func(context.Context, *agentpbv2.PushTarget) (*grpc.ClientConn, error) {
		return grpc.NewClient(l.builder.Target(), grpc.WithTransportCredentials(credentials.NewTLS(l.f.ClientTLS(t, l.f.Builder))))
	}
	spec := &agentpbv2.BuildSpec{AppId: "app", Platform: "linux/arm64", PushTarget: &agentpbv2.PushTarget{AssetUuid: l.f.TargetID.UUID().String(), Repository: "app:latest", RegistryPort: 5000}, Definition: &agentpbv2.BuildSpec_DockerfileBuild{DockerfileBuild: &agentpbv2.DockerfileBuild{Dockerfile: "Dockerfile"}}}
	input, err := buildassignment.InputSHA256(spec)
	if err != nil {
		t.Fatal(err)
	}
	claims := l.f.Claims("app:latest")
	claims.InputSHA256 = input
	compact := l.f.Sign(t, claims)
	spec.PushTarget.DeliveryAuthorizationJws = compact
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := svc.verifyDeliveryAssignments(ctx, spec, []*agentpbv2.PushTarget{spec.PushTarget}); err == nil {
		t.Fatal("unadmitted assignment accepted")
	}
	admitted, err := agentpbv2.NewWendyBuildDeliveryServiceClient(l.operator).AdmitDelivery(ctx, &agentpbv2.AdmitDeliveryRequest{AuthorizationJws: compact})
	if err != nil {
		t.Fatal(err)
	}
	spec.PushTarget.DeliverySession = admitted.GetSession()
	if _, err := svc.verifyDeliveryAssignments(ctx, spec, []*agentpbv2.PushTarget{spec.PushTarget}); err != nil {
		t.Fatal(err)
	}
	spec.Platform = "linux/amd64"
	if _, err := svc.verifyDeliveryAssignments(ctx, spec, []*agentpbv2.PushTarget{spec.PushTarget}); err == nil {
		t.Fatal("changed build accepted")
	}
	spec.Platform = "linux/arm64"
	rotated := delivery.NewManager(t.TempDir(), "localhost:5000/", func() (string, string) { return l.f.OtherBuilder.PEM, l.f.Chain })
	t.Cleanup(rotated.Close)
	svc.deliverySource = rotated.Get
	if _, err := svc.verifyDeliveryAssignments(ctx, spec, []*agentpbv2.PushTarget{spec.PushTarget}); err == nil {
		t.Fatal("rotated builder key accepted")
	}
}

func TestBuildHostConfirmsDurablePublicationAndRejectsSupersededReceipt(t *testing.T) {
	l := newDeliveryRPCLab(t)
	session := l.admit(t, l.f.Claims("app:latest"))
	target := &agentpbv2.PushTarget{Repository: "app:latest", DeliverySession: session}
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{})
	svc.dialTarget = func(context.Context, *agentpbv2.PushTarget) (*grpc.ClientConn, error) {
		return grpc.NewClient(l.builder.Target(), grpc.WithTransportCredentials(credentials.NewTLS(l.f.ClientTLS(t, l.f.Builder))))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.confirmDelivery(ctx, target); err == nil {
		t.Fatal("unfinished transfer reported success")
	}
	snapshot, err := l.manager.Get()
	if err != nil {
		t.Fatal(err)
	}
	output := "sha256:" + strings.Repeat("a", 64)
	if err := snapshot.Store.Publish(session, l.f.Builder.Leaf, "app:latest", output, func(expected, chosen string) error {
		l.backend.mu.Lock()
		defer l.backend.mu.Unlock()
		l.backend.images["localhost:5000/app:latest"] = chosen
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.confirmDelivery(ctx, target); err != nil {
		t.Fatal(err)
	}
	l.backend.mu.Lock()
	l.backend.images["localhost:5000/app:latest"] = "sha256:" + strings.Repeat("b", 64)
	l.backend.mu.Unlock()
	if err := svc.confirmDelivery(ctx, target); err == nil {
		t.Fatal("superseded image reported delivered")
	}
}
