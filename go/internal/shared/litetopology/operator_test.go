package litetopology

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// This is the actual nanopb encoder fixture from reviewed LUI fd53af8, not a
// JSON substitute for the wire protocol. Its reporter device differs from the
// operator's installation, and all uint64 values survive both codecs exactly.
func operatorFixture(t *testing.T) *lite.WendyComNetTopologyPage {
	t.Helper()
	raw, err := os.ReadFile("testdata/topology_operator_page.hex")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	message := new(lite.WendyComMessage)
	if err := proto.Unmarshal(wire, message); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil || !bytes.Equal(wire, encoded) {
		t.Fatal("actual C and Go binary codecs differ", err)
	}
	return message.GetResponse().GetNetTopology()
}

func typedOperatorPages(t *testing.T) []*lite.WendyComNetTopologyPage {
	t.Helper()
	first := operatorFixture(t)
	rows := []*lite.WendyComNetTopologyRecord{proto.Clone(first.Record).(*lite.WendyComNetTopologyRecord)}
	second := proto.Clone(first.Record).(*lite.WendyComNetTopologyRecord)
	instance := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	operator, err := meshoperator.ParsePrincipal(first.Record.OwnerPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := meshoperator.Endpoint{Operator: operator, Instance: meshoperator.InstanceID(instance)}
	address := endpoint.Address().As16()
	second.OwnerInstance, second.Address = instance[:], address[:]
	second.RouterId, second.Revision = 9, 5
	rows = append(rows, second)
	gateway := uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	c6 := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	for _, announcement := range rows[:2] {
		route := proto.Clone(announcement).(*lite.WendyComNetTopologyRecord)
		route.Kind, route.Metric = 2, 128
		route.NextHopDevice, route.LinkId = gateway[:], math.MaxUint64
		route.ExpiresAtUnixMs = first.CapturedAtUnixMs + 300_000
		rows = append(rows, route)
	}
	for index, id := range []uuid.UUID{gateway, c6} {
		address := meshid.MeshAddressFor(id).As16()
		rows = append(rows, &lite.WendyComNetTopologyRecord{Kind: 1, OwnerRole: 1, Device: id[:], Address: address[:], LinkId: math.MaxUint64 - uint64(index), Metric: 128 + uint32(index)*8064, Transport: uint32(index + 1)})
	}
	unknown := netip.MustParseAddr("fd45:b891:5680::99").As16()
	rows = append(rows, &lite.WendyComNetTopologyRecord{Kind: 2, Address: unknown[:], RouterId: 17, Metric: 65535, Unreachable: true})
	pages := make([]*lite.WendyComNetTopologyPage, len(rows))
	for index, row := range rows {
		page := proto.Clone(first).(*lite.WendyComNetTopologyPage)
		page.Record = row
		if index+1 < len(rows) {
			page.NextCursor = uint32(index + 1)
		}
		pages[index] = page
	}
	return pages
}

func normalizedOperatorFixture(t *testing.T) *pb.LocalMeshTopology {
	t.Helper()
	pages := typedOperatorPages(t)
	snapshot, err := Read(context.Background(), func(_ context.Context, request *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		return proto.Clone(pages[request.Cursor]).(*lite.WendyComNetTopologyPage), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestTypedOperatorNanopbProductionNormalization(t *testing.T) {
	snapshot := normalizedOperatorFixture(t)
	if snapshot.SelfDeviceId != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" || snapshot.Status.State != "mesh-active" || snapshot.Status.Configured != nil {
		t.Fatal("missing policy must remain unknown", snapshot)
	}
	if len(snapshot.Announcements) != 2 || len(snapshot.Routes) != 3 || len(snapshot.Peers) != 2 {
		t.Fatal(snapshot)
	}
	first, second := snapshot.Announcements[0], snapshot.Announcements[1]
	if first.Principal != second.Principal || !strings.HasSuffix(first.Principal, "/operator/Alice") || first.DeviceId != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || second.DeviceId != "cccccccc-cccc-4ccc-8ccc-cccccccccccc" || first.Prefix == second.Prefix {
		t.Fatal("operator installations were conflated or recast as devices", first, second)
	}
	if first.OriginRouterId != math.MaxUint64-1 || first.Revision != math.MaxUint64 || snapshot.Peers[0].LinkId != math.MaxUint64 {
		t.Fatal("uint64 precision lost", snapshot)
	}
	unknown := snapshot.Routes[2]
	if unknown.Prefix != "fd45:b891:5680::99/128" || unknown.OriginPrincipal != "" || unknown.OriginAuthorized || !unknown.Unreachable {
		t.Fatal("ordinary address-only Babel hold route was misrepresented", unknown)
	}
	raw, err := protojson.MarshalOptions{Indent: "  "}.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{first.DeviceId, second.DeviceId, "18446744073709551615", "18446744073709551614", "fd45:b891:5680:cca:1cd7:f65e:b581:101b/128"} {
		if !bytes.Contains(raw, []byte(text)) {
			t.Fatal(string(raw))
		}
	}
	if bytes.Contains(raw, []byte("ownerInstance")) || bytes.Contains(raw, []byte("qqqqqqqqSqqKqqqqqqqqqg==")) || bytes.Contains(raw, []byte("agentPort")) {
		t.Fatal("raw bytes or an invented agent endpoint reached UI JSON", string(raw))
	}
	if output := os.Getenv("WENDY_GENERATE_LITE_OPERATOR_FIXTURE"); output != "" {
		var document any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		canonical, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, append(canonical, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnavailableLiteRuntimeDoesNotManufactureRelationships(t *testing.T) {
	page := operatorFixture(t)
	page.RuntimeAvailable = false
	// A prior row retained during an ordinary stop must not create a live edge
	// or authority when the runtime is explicitly unavailable.
	snapshot, err := Read(context.Background(), func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		return page, nil
	})
	if err != nil || snapshot.RuntimeAvailable || len(snapshot.Peers)+len(snapshot.Routes)+len(snapshot.Announcements) != 0 {
		t.Fatal(snapshot, err)
	}
}

func TestTypedOperatorNormalMaximumOpaqueSubject(t *testing.T) {
	page := operatorFixture(t)
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/Team%2FUser%22Name%5C"
	principal += strings.Repeat("x", meshoperator.MaxPrincipalBytes-len(principal))
	operator, err := meshoperator.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	instance := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	endpoint := meshoperator.Endpoint{Operator: operator, Instance: meshoperator.InstanceID(instance)}
	address := endpoint.Address().As16()
	page.Record.OwnerPrincipal, page.Record.Address = principal, address[:]
	snapshot, err := Read(context.Background(), func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		return page, nil
	})
	if err != nil || len(snapshot.GetAnnouncements()) != 1 || snapshot.Announcements[0].Principal != principal || snapshot.Announcements[0].DeviceId != instance.String() {
		t.Fatal(snapshot, err)
	}
}

func TestOperatorNeighbourResolvesRouteNextHopByLink(t *testing.T) {
	pages := typedOperatorPages(t)
	// The first typed operator announcement also describes its direct carrier.
	peer := proto.Clone(pages[0].Record).(*lite.WendyComNetTopologyRecord)
	peer.Kind = 1
	peer.LinkId = 91
	peer.Transport = 1
	peer.Metric = 128
	pages[4].Record = peer
	pages[2].Record.LinkId = 91
	pages[2].Record.NextHopDevice = nil
	snapshot, err := Read(context.Background(), func(_ context.Context, p *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		return proto.Clone(pages[p.Cursor]).(*lite.WendyComNetTopologyPage), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	route := snapshot.Routes[0]
	addr, _ := address(peer.Address)
	if route.NextHopPrincipal != peer.OwnerPrincipal || route.NextHopAddress != addr {
		t.Fatalf("typed operator next hop lost: %+v", route)
	}
}
