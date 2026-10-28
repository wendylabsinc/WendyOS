package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func operatorTopologyFixture(t *testing.T) *pb.LocalMeshTopology {
	t.Helper()
	self, err := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/Alice")
	if err != nil {
		t.Fatal(err)
	}
	self, err = self.WithInstance(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	peer, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1793000000000)
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: babel.RouterID(math.MaxUint64 - 1), Seqno: 42}
	snapshot := localmesh.NodeSnapshot{Self: self, CapturedAt: now, Peers: 1, Links: []localmesh.PeerLink{{ID: babel.LinkID(math.MaxUint64), Device: peer, Cost: 8192, Transport: "ble"}},
		Devices: []localmesh.Manifest{{Version: localmesh.ManifestVersionRoute, Device: peer.UUID(), Revision: math.MaxUint64, Issued: now.Add(-time.Second).UnixMilli(), Expires: now.Add(5 * time.Minute).UnixMilli(), Name: "C6 neighbor", Platform: "Lite", AgentPort: 5054, AgentTransport: localmesh.AgentMTLS, Route: &origin}},
		Routes:  []babel.Route{{Prefix: netip.PrefixFrom(self.MeshAddress(), 128), RouterID: 7, Local: true}, {Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Metric: 8192, Link: babel.LinkID(math.MaxUint64), NextHop: peer.MeshAddress()}}, RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true}}
	other, err := self.WithInstance(uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"))
	if err != nil {
		t.Fatal(err)
	}
	otherOrigin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(other.MeshAddress(), 128), RouterID: 9, Seqno: 1}
	snapshot.Links = append(snapshot.Links, localmesh.PeerLink{ID: 9, Device: other, Cost: 128, Transport: "ethernet"})
	snapshot.Devices = append(snapshot.Devices, localmesh.Manifest{Version: localmesh.ManifestVersionRoute, Owner: &localmesh.OperatorOwner{Principal: other.Principal(), Instance: other.UUID(), Hashes: [][]byte{make([]byte, 32)}}, Revision: 3, Issued: now.Add(-time.Second).UnixMilli(), Expires: now.Add(5 * time.Minute).UnixMilli(), Name: "Other CLI installation", Route: &otherOrigin})
	snapshot.Routes = append(snapshot.Routes, babel.Route{Prefix: otherOrigin.Prefix, RouterID: 9, Seqno: 1, Metric: 128, Link: 9, NextHop: other.MeshAddress()})
	snapshot.RouteAuthorizations[otherOrigin] = true
	return meshview.Topology(self, snapshot, now)
}
func TestLocalCLIRouterObservationUsesPrivateOwnerAndNormalizedModel(t *testing.T) {
	oldRead, oldResolve, oldJSON := readLocalOperatorTopology, resolveLocalMeshTopologyTarget, jsonOutput
	defer func() {
		readLocalOperatorTopology, resolveLocalMeshTopologyTarget, jsonOutput = oldRead, oldResolve, oldJSON
	}()
	snapshot := operatorTopologyFixture(t)
	calls := 0
	readLocalOperatorTopology = func(ctx context.Context) (*pb.LocalMeshTopology, error) {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
			t.Fatal("private read is unbounded")
		}
		return proto.Clone(snapshot).(*pb.LocalMeshTopology), nil
	}
	resolveLocalMeshTopologyTarget = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		t.Fatal("local owner read opened a device target")
		return nil, nil
	}
	jsonOutput = true
	cmd := newDeviceLocalMeshTopologyCmd()
	cmd.SetArgs([]string{"--local-cli", "--observation"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var observation struct {
		Status, Platform string
		Topology         map[string]json.RawMessage
	}
	if err := json.Unmarshal(output.Bytes(), &observation); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || observation.Status != "ok" || observation.Platform != "wendy-cli" {
		t.Fatal(calls, output.String())
	}
	var principal, address, instance string
	json.Unmarshal(observation.Topology["selfPrincipal"], &principal)
	json.Unmarshal(observation.Topology["selfMeshAddress"], &address)
	json.Unmarshal(observation.Topology["selfDeviceId"], &instance)
	if principal != snapshot.SelfPrincipal || address != snapshot.SelfMeshAddress || instance != snapshot.SelfDeviceId || !bytes.Contains(output.Bytes(), []byte(`"18446744073709551615"`)) {
		t.Fatal(output.String())
	}
	if path := os.Getenv("WENDY_GENERATE_LOCAL_TOPOLOGY_FIXTURE"); path != "" {
		raw, err := protojson.MarshalOptions{Indent: "  "}.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func TestAbsentOrOffLocalCLIReportsUnavailableWithoutTargetResolution(t *testing.T) {
	oldRead, oldResolve, oldJSON := readLocalOperatorTopology, resolveLocalMeshTopologyTarget, jsonOutput
	defer func() {
		readLocalOperatorTopology, resolveLocalMeshTopologyTarget, jsonOutput = oldRead, oldResolve, oldJSON
	}()
	resolveLocalMeshTopologyTarget = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		t.Fatal("absent/off read started device resolution")
		return nil, nil
	}
	for _, readErr := range []error{sessionbroker.ErrUnavailable, status.Error(codes.Unavailable, "operator mesh is explicitly off")} {
		readLocalOperatorTopology = func(context.Context) (*pb.LocalMeshTopology, error) { return nil, readErr }
		jsonOutput = true
		cmd := newDeviceLocalMeshTopologyCmd()
		cmd.SetArgs([]string{"--local-cli", "--observation"})
		var output bytes.Buffer
		cmd.SetOut(&output)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result["status"] != "unavailable" || result["platform"] != "wendy-cli" || result["topology"] != nil {
			t.Fatal(result)
		}
	}
}
