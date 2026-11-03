package commands

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// scriptedMeshPairingConsumer exposes the selected consumer's pairing RPCs.
// Sources are the consumer's signed mesh projection, not a CLI-local scan.
type scriptedMeshPairingConsumer struct {
	agentpbv2.UnimplementedWendySensorPairingServiceServer
	mu          sync.Mutex
	sources     []*agentpbv2.DiscoveredSensorSource
	pairings    []*agentpbv2.SensorPairing
	added       *agentpbv2.AddSensorPairingRequest
	removed     *agentpbv2.RemoveSensorPairingRequest
	discoveries int
}

func (s *scriptedMeshPairingConsumer) DiscoverSensorSources(context.Context, *agentpbv2.DiscoverSensorSourcesRequest) (*agentpbv2.DiscoverSensorSourcesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoveries++
	return &agentpbv2.DiscoverSensorSourcesResponse{Sources: s.sources}, nil
}

func (s *scriptedMeshPairingConsumer) ListSensorPairings(context.Context, *agentpbv2.ListSensorPairingsRequest) (*agentpbv2.ListSensorPairingsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &agentpbv2.ListSensorPairingsResponse{Pairings: s.pairings, SupportsPrincipalPairing: true}, nil
}

func (s *scriptedMeshPairingConsumer) AddSensorPairing(_ context.Context, req *agentpbv2.AddSensorPairingRequest) (*agentpbv2.AddSensorPairingResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.added = proto.Clone(req).(*agentpbv2.AddSensorPairingRequest)
	pairing := &agentpbv2.SensorPairing{
		SourceAssetId: req.SourceAssetId, SourcePrincipalUri: req.SourcePrincipalUri,
		Name: req.Name, SensorAllowlist: req.SensorAllowlist, Transport: req.Transport,
	}
	s.pairings = []*agentpbv2.SensorPairing{pairing}
	return &agentpbv2.AddSensorPairingResponse{Pairing: pairing}, nil
}

func (s *scriptedMeshPairingConsumer) RemoveSensorPairing(_ context.Context, req *agentpbv2.RemoveSensorPairingRequest) (*agentpbv2.RemoveSensorPairingResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = proto.Clone(req).(*agentpbv2.RemoveSensorPairingRequest)
	s.pairings = nil
	return &agentpbv2.RemoveSensorPairingResponse{}, nil
}

// This exercises the scripted operations through a real selected-consumer gRPC
// connection. It does not test mesh:// selection or broker transport: those are
// supplied by the later CLI layer. The consumer must receive source identity,
// leaving LAN/mesh address selection to its own resolver.
func TestScriptedSensorPairMeshSourceOverConsumerGRPC(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444"
	signed := models.LANDevice{
		ID: "signed-source", DisplayName: "Mesh camera", IPAddress: "fd45:b891:567f::5", Port: 5055,
		TenantUUID: tenant, PrincipalURI: principal, IsMTLS: true, WendyLite: true, Sensorlink: true, Caps: []string{"sensors"},
	}
	lan := signed
	lan.DisplayName, lan.Hostname, lan.IPAddress = "LAN camera", "board.local", "192.0.2.20"
	sources := discovery.MergeSensorSourcesWithMesh([]models.LANDevice{lan}, []models.LANDevice{signed})
	if len(sources) != 1 {
		t.Fatalf("consumer should project one canonical signed source: %+v", sources)
	}
	for _, query := range []string{signed.DisplayName, principal} {
		t.Run(query, func(t *testing.T) {
			consumer := &scriptedMeshPairingConsumer{sources: []*agentpbv2.DiscoveredSensorSource{deviceSource(sources[0])}}
			client := pairingClientOver(t, consumer)
			h := &sensorPairHandler{
				ctx: context.Background(), client: client, name: "Front camera", sensors: []string{"video"},
				tenantIDs: func() (map[string]bool, error) { return map[string]bool{tenant: true}, nil },
				discoverLocal: func(context.Context) ([]models.DiscoveredDevice, error) {
					t.Error("scripted pairing scanned the CLI's network instead of the selected consumer")
					return nil, nil
				},
			}
			var out bytes.Buffer
			if err := runSensorPair(context.Background(), h, query, true, &out); err != nil {
				t.Fatal(err)
			}
			var paired agentpbv2.AddSensorPairingResponse
			if err := protojson.Unmarshal(out.Bytes(), &paired); err != nil {
				t.Fatalf("pair JSON: %v\n%s", err, out.String())
			}
			want := &agentpbv2.SensorPairing{SourcePrincipalUri: principal, Name: "Front camera", SensorAllowlist: []string{"video"}, Transport: "wendycom"}
			if !proto.Equal(paired.Pairing, want) {
				t.Fatalf("pairing = %v, want %v (saving intent does not establish a sensor stream)", paired.Pairing, want)
			}
			out.Reset()
			if err := runSensorList(context.Background(), client, true, &out); err != nil {
				t.Fatal(err)
			}
			var listed agentpbv2.ListSensorPairingsResponse
			if err := protojson.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Pairings) != 1 || !proto.Equal(listed.Pairings[0], want) {
				t.Fatalf("saved pairing list: %v\n%s", err, out.String())
			}
			out.Reset()
			if err := runSensorForget(context.Background(), h, principal, true, &out); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := runSensorList(context.Background(), client, true, &out); err != nil {
				t.Fatal(err)
			}
			if err := protojson.Unmarshal(out.Bytes(), &listed); err != nil || len(listed.Pairings) != 0 {
				t.Fatalf("forgotten pairing remains: %v\n%s", err, out.String())
			}
			consumer.mu.Lock()
			defer consumer.mu.Unlock()
			wantAdd := &agentpbv2.AddSensorPairingRequest{SourcePrincipalUri: principal, Name: want.Name, SensorAllowlist: want.SensorAllowlist, Transport: "wendycom"}
			if !proto.Equal(consumer.added, wantAdd) || consumer.added.GetSourceAddress() != "" {
				t.Fatalf("consumer-relative pairing must send exact identity without a pinned LAN or VIP address: %v", consumer.added)
			}
			if consumer.discoveries != 1 || consumer.removed.GetSourcePrincipalUri() != principal || consumer.removed.GetSourceAssetId() != 0 {
				t.Fatalf("forget must use saved principal without rediscovery: scans=%d remove=%v", consumer.discoveries, consumer.removed)
			}
		})
	}
}
