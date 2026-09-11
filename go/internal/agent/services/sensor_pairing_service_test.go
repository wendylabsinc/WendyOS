package services_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

func TestAddAndListSensorPairing(t *testing.T) {
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "p.json"))
	_ = store.Load()
	started := map[int32]string{}
	streaming := map[int32]bool{}
	const agentOrgID = int32(42)
	svc := services.NewSensorPairingService(zap.NewNop(), store, func() int32 { return agentOrgID },
		func(p mcusource.SensorPairing, addr string) { started[p.SourceAssetID] = addr },
		nil,
		func(sourceAssetID int32) bool { return streaming[sourceAssetID] })

	addResp, err := svc.AddSensorPairing(context.Background(), &agentpbv2.AddSensorPairingRequest{SourceAssetId: 7, SourceAddress: "1.2.3.4:7000", Name: "hub", Transport: "grpc"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if addResp.Pairing.OrgId != agentOrgID {
		t.Fatalf("expected returned pairing to carry the agent's org id %d, got %d", agentOrgID, addResp.Pairing.OrgId)
	}
	if addResp.Pairing.Transport != "grpc" {
		t.Fatalf("expected Add response to round-trip transport=grpc, got %q", addResp.Pairing.Transport)
	}
	if started[7] != "1.2.3.4:7000" {
		t.Fatalf("supervisor not started: %v", started)
	}
	resp, err := svc.ListSensorPairings(context.Background(), &agentpbv2.ListSensorPairingsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Pairings) != 1 || resp.Pairings[0].SourceAssetId != 7 {
		t.Fatalf("bad list: %+v", resp.Pairings)
	}
	if resp.Pairings[0].OrgId != agentOrgID {
		t.Fatalf("expected stored pairing to carry the agent's org id %d, got %d", agentOrgID, resp.Pairings[0].OrgId)
	}
	if resp.Pairings[0].Transport != "grpc" {
		t.Fatalf("expected listed pairing to carry transport=grpc, got %q", resp.Pairings[0].Transport)
	}
	if resp.Pairings[0].Connected {
		t.Fatalf("expected asset 7 to list as not connected before it delivers frames")
	}

	// Mark asset 7 as delivering frames and add a second, still-connecting pairing.
	streaming[7] = true
	if _, err := svc.AddSensorPairing(context.Background(), &agentpbv2.AddSensorPairingRequest{SourceAssetId: 8, SourceAddress: "5.6.7.8:7000", Name: "other"}); err != nil {
		t.Fatalf("add second: %v", err)
	}

	resp, err = svc.ListSensorPairings(context.Background(), &agentpbv2.ListSensorPairingsRequest{})
	if err != nil {
		t.Fatalf("list after streaming: %v", err)
	}
	connected := map[int32]bool{}
	for _, p := range resp.Pairings {
		connected[p.SourceAssetId] = p.Connected
	}
	if !connected[7] {
		t.Fatalf("expected asset 7 (streaming) to list as connected: %+v", resp.Pairings)
	}
	if connected[8] {
		t.Fatalf("expected asset 8 (still connecting) to list as not connected: %+v", resp.Pairings)
	}
	for _, p := range resp.Pairings {
		if p.SourceAssetId == 8 && p.Transport != "" {
			t.Fatalf("expected asset 8 (no transport on Add) to list with empty transport (tcp back-compat default), got %q", p.Transport)
		}
	}
}
