package services_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWorldNativeCameraPairingUsesTenantAndExactAddress(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	principal := certs.DeviceSPIFFEURI(tenant, "gateway-a")
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "pairings.json"))
	var started mcusource.SensorPairing
	var address string
	svc := services.NewSensorPairingService(zap.NewNop(), store,
		func() certs.Scope { return certs.Scope{TenantUUID: tenant} },
		func(p mcusource.SensorPairing, addr string) { started, address = p, addr }, nil, nil)
	foreign := certs.DeviceSPIFFEURI("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "gateway-a")
	_, err := svc.AddSensorPairing(context.Background(), &agentpbv2.AddSensorPairingRequest{SourcePrincipalUri: foreign, Transport: "tcp", SourceAddress: "127.0.0.1:50060"})
	if status.Code(err) != codes.PermissionDenied || len(store.List()) != 0 || started.SourcePrincipalURI != "" {
		t.Fatalf("foreign tenant changed pairing: %v", err)
	}
	reply, err := svc.AddSensorPairing(context.Background(), &agentpbv2.AddSensorPairingRequest{SourcePrincipalUri: principal, Transport: "tcp", SourceAddress: "127.0.0.1:50060", SensorAllowlist: []string{"camera-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if started.SourcePrincipalURI != principal || started.Transport != "tcp" || address != "127.0.0.1:50060" || reply.Pairing.Connected || len(store.List()) != 1 {
		t.Fatalf("wrong native pairing: %+v %q %+v", started, address, reply)
	}
}
