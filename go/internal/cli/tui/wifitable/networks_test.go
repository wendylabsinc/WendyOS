package wifitable

import (
	"reflect"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestSortKnownThenUnknown(t *testing.T) {
	networks := []Network{
		{SSID: "UnknownA", Signal: 30},
		{SSID: "KnownLow", Known: true, Priority: 1, Signal: 40},
		{SSID: "UnknownB", Signal: 80},
		{SSID: "KnownHigh", Known: true, Priority: 10, Signal: 50},
		{SSID: "KnownMid", Known: true, Priority: 5, Signal: 10},
		{SSID: "Active", Known: true, Connected: true, Priority: 0, Signal: 90},
	}
	Sort(networks)

	want := []string{"Active", "KnownHigh", "KnownMid", "KnownLow", "UnknownB", "UnknownA"}
	var got []string
	for _, n := range networks {
		got = append(got, n.SSID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sort order = %v; want %v", got, want)
	}
}

func TestMoveUpDownOnlyWithinKnownBlock(t *testing.T) {
	networks := []Network{
		{SSID: "A", Known: true, Priority: 10},
		{SSID: "B", Known: true, Priority: 5},
		{SSID: "C", Known: true, Priority: 1},
		{SSID: "D", Signal: 70},
	}

	// Moving the top known network up does nothing.
	if idx := MoveUp(networks, 0); idx != 0 {
		t.Errorf("MoveUp(0) idx = %d; want 0", idx)
	}

	// Moving B up swaps with A.
	if idx := MoveUp(networks, 1); idx != 0 {
		t.Errorf("MoveUp(1) idx = %d; want 0", idx)
	}
	if networks[0].SSID != "B" || networks[1].SSID != "A" {
		t.Fatalf("unexpected order after MoveUp(1): %v", ssids(networks))
	}

	// Moving the last known network down into the unknown block is blocked.
	if idx := MoveDown(networks, 2); idx != 2 {
		t.Errorf("MoveDown(2) idx = %d; want 2 (blocked by unknown row)", idx)
	}
	if networks[2].SSID != "C" || networks[3].SSID != "D" {
		t.Fatalf("MoveDown(2) should not have swapped: %v", ssids(networks))
	}

	// Moving an unknown network does nothing.
	if idx := MoveUp(networks, 3); idx != 3 {
		t.Errorf("MoveUp(3) on unknown row idx = %d; want 3", idx)
	}
}

func TestKnownSSIDsInOrder(t *testing.T) {
	networks := []Network{
		{SSID: "K1", Known: true},
		{SSID: "K2", Known: true},
		{SSID: "U1"},
		{SSID: "K3", Known: true},
	}
	got := KnownSSIDsInOrder(networks)
	want := []string{"K1", "K2", "K3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("KnownSSIDsInOrder = %v; want %v", got, want)
	}
}

func TestFromProtoPopulatesFields(t *testing.T) {
	signal := int32(62)
	priority := int32(3)
	pb := &agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		Ssid:           "Home",
		SignalStrength: &signal,
		Security:       agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK,
		IsKnown:        true,
		IsConnected:    true,
		Priority:       &priority,
	}
	n := FromProto(pb)
	if n.SSID != "Home" || n.Signal != 62 || !n.Known || !n.Connected || n.Priority != 3 {
		t.Fatalf("FromProto returned unexpected fields: %+v", n)
	}
	if n.Security != agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK {
		t.Errorf("security = %v", n.Security)
	}
}

func ssids(ns []Network) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.SSID
	}
	return out
}
