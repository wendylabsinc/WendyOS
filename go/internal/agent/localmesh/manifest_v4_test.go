package localmesh

import (
	"bytes"
	"crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

// A future V4 origin adds harmless metadata at all supported map layers. The
// current reader still verifies its normal route and known sensors capability,
// while forwarding and restarting with the exact original signed body bytes.
func TestManifestV4OptionalMetadataSurvivesDirectoryAndRestart(t *testing.T) {
	directory, chain, key, device, now := directoryFixture(t)
	manifest := routeManifest(device, now)
	manifest.Platform = PlatformLite
	manifest.AgentTransport = AgentMTLS
	manifest.Caps = []string{CapabilitySensors, "vendor/future"}
	manifest.AppEndpoints = []AppEndpoint{{Transport: AppTLS, Port: AppSessionTLSPort}}
	wire, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]netwire.RawMessage
	if err := netwire.Unmarshal(wire.Body, &fields); err != nil {
		t.Fatal(err)
	}
	encode := func(value any) netwire.RawMessage {
		t.Helper()
		data, err := netwire.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	fields["futureDescription"] = encode(map[string]any{"label": "future", "optional": nil})
	var route map[string]netwire.RawMessage
	if err := netwire.Unmarshal(fields[netwire.Key("route")], &route); err != nil {
		t.Fatal(err)
	}
	route["futureCost"] = encode(uint64(10))
	fields[netwire.Key("route")] = encode(route)
	var endpoints []map[string]netwire.RawMessage
	if err := netwire.Unmarshal(fields[netwire.Key("appEndpoints")], &endpoints); err != nil {
		t.Fatal(err)
	}
	endpoints[0]["futureInterface"] = encode(map[string]any{"index": uint64(3)})
	fields[netwire.Key("appEndpoints")] = encode(endpoints)
	wire.Body = encode(fields)
	input, opts := signingInput(key.Public(), wire.Principal, wire.Body)
	wire.Signature, err = key.Sign(rand.Reader, input, opts)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []DirectoryReceipt
	persist := func(next []DirectoryReceipt) error { receipts = append([]DirectoryReceipt(nil), next...); return nil }
	if err := directory.SetPersistence(nil, persist, now); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	view := directory.Snapshot(now)
	if len(view) != 1 || !slices.Contains(view[0].Caps, CapabilitySensors) || !slices.Contains(view[0].Caps, "vendor/future") || !directory.RouteAuthorizations(now)[*manifest.Route] {
		t.Fatalf("normal future metadata view: %+v", view)
	}
	checkBytes := func(d *Directory) {
		t.Helper()
		records := d.Records(now)
		if len(records) != 1 || !bytes.Equal(records[0].Body, wire.Body) {
			t.Fatalf("original signed bytes changed: %+v", records)
		}
	}
	checkBytes(directory)
	sync := NewSynchronizer(directory, directory.cache, netgossip.NewPool())
	if have := sync.Record(wire, now); len(have) != 1 || have[0].Gossip == nil {
		t.Fatalf("future metadata not offered for ordinary gossip: %+v", have)
	}
	restored, err := NewDirectory(device.Tenant(), 10, directory.cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.SetPersistence(receipts, persist, now); err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot(now)) != 0 {
		t.Fatal("replay receipts restored active presence")
	}
	if _, err := restored.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	checkBytes(restored)
}

func TestManifestV4OptionalDefaultsAndStableCapabilityOrder(t *testing.T) {
	_, chain, key, device, now := directoryFixture(t)
	manifest := routeManifest(device, now)
	manifest.Caps = []string{"vendor/future", CapabilitySensors}
	wire, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := DecodeManifest(wire.Body)
	if err != nil || !slices.Equal(view.Caps, []string{CapabilitySensors, "vendor/future"}) {
		t.Fatalf("stable capability order: %+v %v", view, err)
	}
	if manifest.Caps[0] != "vendor/future" {
		t.Fatal("signing mutated caller capability storage")
	}
	manifest.Caps = []string{}
	empty, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	emptyView, err := DecodeManifest(empty.Body)
	if err != nil || emptyView.Caps == nil || len(emptyView.Caps) != 0 {
		t.Fatalf("explicit no capabilities lost: %+v %v", emptyView, err)
	}
	var fields map[string]netwire.RawMessage
	if err := netwire.Unmarshal(wire.Body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"caps", "requires", "name", "platform", "appEndpoints"} {
		delete(fields, netwire.Key(field))
	}
	body, err := netwire.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	view, err = DecodeManifest(body)
	if err != nil || view.Caps != nil || len(view.Requires) != 0 || view.AppEndpoints == nil || len(view.AppEndpoints) != 0 {
		t.Fatalf("optional defaults: %+v %v", view, err)
	}
}

func TestManifestV4RetainsStoredReplayFloor(t *testing.T) {
	directory, chain, key, device, now := directoryFixture(t)
	floor := DirectoryReceipt{Device: device.Principal(), Revision: 42, Hash: strings.Repeat("a", 64), Until: now.Add(MaxLease)}
	var saved []DirectoryReceipt
	if err := directory.SetPersistence([]DirectoryReceipt{floor}, func(next []DirectoryReceipt) error { saved = append([]DirectoryReceipt(nil), next...); return nil }, now); err != nil {
		t.Fatal(err)
	}
	if directory.history[device.Principal()] != floor || len(directory.Snapshot(now)) != 0 {
		t.Fatal("restart changed replay floor or restored an old body")
	}
	manifest := routeManifest(device, now)
	manifest.Revision = 43
	wire, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Revision != 43 || !saved[0].Until.After(now.Add(time.Minute)) {
		t.Fatalf("new v4 revision not persisted over existing floor: %+v", saved)
	}
}
