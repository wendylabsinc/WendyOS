package network

import (
	"context"
	"testing"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestParseWiFiProfile(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want wifiProfile
	}{
		{"open network", ssidField + ":Cafe\n", wifiProfile{SSID: "Cafe"}},
		{"psk with forced open auth", profileOut("wpa-psk", "open", "Home"),
			wifiProfile{Mode: "infrastructure", SSID: "Home", KeyMgmt: "wpa-psk", AuthAlg: "open"}},
		{"colon and backslash kept raw", profileOut("wpa-psk", "", `a:b\c`),
			wifiProfile{Mode: "infrastructure", SSID: `a:b\c`, KeyMgmt: "wpa-psk"}},
		{"newline and blank line in ssid", profileOut("sae", "", "zz\n\nnl"),
			wifiProfile{Mode: "infrastructure", SSID: "zz\n\nnl", KeyMgmt: "sae"}},
		{"ssid that looks like a field", profileOut("sae", "", "x\n"+keyMgmtField+":wpa-psk"),
			wifiProfile{Mode: "infrastructure", SSID: "x\n" + keyMgmtField + ":wpa-psk", KeyMgmt: "sae"}},
		{"surrounding spaces kept", profileOut("wpa-psk", "open", " zz psk "),
			wifiProfile{Mode: "infrastructure", SSID: " zz psk ", KeyMgmt: "wpa-psk", AuthAlg: "open"}},
		{"trailing newline in ssid", profileOut("", "", "zz\n"), wifiProfile{Mode: "infrastructure", SSID: "zz\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseWiFiProfile(c.out)
			if err != nil || got != c.want {
				t.Fatalf("parseWiFiProfile(%q) = %+v, %v; want %+v", c.out, got, err, c.want)
			}
		})
	}
	if _, err := parseWiFiProfile("Error: something else\n"); err == nil {
		t.Error("unexpected output must be an error")
	}
}

// Several saved profiles: the lookup must match each SSID to its own UUID.
func severalProfiles() []reply {
	return []reply{
		{args: "-t -f UUID,TYPE connection show", out: "uuid-a:802-11-wireless\nuuid-eth:802-3-ethernet\nuuid-b:802-11-wireless\nuuid-c:802-11-wireless\n"},
		{args: "-t -f NAME,UUID,TYPE,AUTOCONNECT-PRIORITY connection show",
			out: "Home:uuid-a:802-11-wireless:5\nWired:uuid-eth:802-3-ethernet:0\nCafe:uuid-b:802-11-wireless:0\nOffice:uuid-c:802-11-wireless:2\n"},
		{args: batchArgs("uuid-a", "uuid-b", "uuid-c"), out: batchOut(
			batchRecord("uuid-a", "wpa-psk", "", "Home"),
			batchRecord("uuid-b", "", "", "Cafe"),
			batchRecord("uuid-c", "sae", "", "Office"))},
	}
}

func TestReadWiFiProfilesInOneCall(t *testing.T) {
	odd := "a\n\n" + uuidField + ":x"
	path, log := writeFakeNMCLI(t, reply{args: batchArgs("u1", "u2", "u3"), out: batchOut(
		batchRecord("u1", "wpa-psk", "open", odd),
		batchRecord("u2", "", "", "zz\n"),
		batchRecord("u3", "sae", "", "Office"))})
	got, err := readWiFiProfiles(context.Background(), path, []string{"u1", "u2", "u3"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]wifiProfile{
		"u1": {Mode: "infrastructure", SSID: odd, KeyMgmt: "wpa-psk", AuthAlg: "open"},
		"u2": {Mode: "infrastructure", SSID: "zz\n"},
		"u3": {Mode: "infrastructure", SSID: "Office", KeyMgmt: "sae"},
	}
	for uuid, w := range want {
		if got[uuid] != w {
			t.Errorf("%s = %+v; want %+v", uuid, got[uuid], w)
		}
	}
	if calls := readCalls(t, log); len(calls) != 1 {
		t.Errorf("made %d nmcli calls %q; want one", len(calls), calls)
	}
}

// Output that doesn't follow the requested order is read again one profile at a time.
func TestReadWiFiProfilesFallsBackOnUnexpectedOrder(t *testing.T) {
	path, _ := writeFakeNMCLI(t,
		reply{args: batchArgs("u1", "u2"), out: batchOut(batchRecord("u2", "", "", "B"), batchRecord("u1", "", "", "A"))},
		reply{args: profileArgs("u1"), out: profileOut("", "", "A")},
		reply{args: profileArgs("u2"), out: profileOut("", "", "B")},
	)
	got, err := readWiFiProfiles(context.Background(), path, []string{"u1", "u2"})
	if err != nil || got["u1"].SSID != "A" || got["u2"].SSID != "B" {
		t.Fatalf("readWiFiProfiles = %+v, %v; want u1=A u2=B", got, err)
	}
}

func TestExistingProfileUUIDWithSeveralProfiles(t *testing.T) {
	path, _ := writeFakeNMCLI(t, severalProfiles()...)
	for ssid, want := range map[string]string{"Home": "uuid-a", "Cafe": "uuid-b", "Office": "uuid-c", "Elsewhere": ""} {
		got, err := existingProfileUUID(context.Background(), path, ssid)
		if err != nil || got != want {
			t.Errorf("existingProfileUUID(%q) = %q, %v; want %q", ssid, got, err, want)
		}
	}
}

func TestExistingProfileUUIDSkipsVanishedProfile(t *testing.T) {
	path, _ := writeFakeNMCLI(t,
		reply{args: "-t -f UUID,TYPE connection show", out: "uuid-gone:802-11-wireless\nuuid-a:802-11-wireless\n"},
		reply{args: batchArgs("uuid-gone", "uuid-a"), out: batchRecord("uuid-a", "wpa-psk", "", "Home"), code: nmcliNoSuchConnection},
		reply{args: profileArgs("uuid-gone"), out: "Error: uuid-gone - no such connection profile.\n", code: nmcliNoSuchConnection},
		reply{args: profileArgs("uuid-a"), out: profileOut("wpa-psk", "", "Home")},
	)
	got, err := existingProfileUUID(context.Background(), path, "Home")
	if err != nil || got != "uuid-a" {
		t.Fatalf("existingProfileUUID = %q, %v; want uuid-a", got, err)
	}
}

// A profile that can't be read must not be taken as absent: the caller would
// create a duplicate.
func TestExistingProfileUUIDFailsOnUnreadableProfile(t *testing.T) {
	path, _ := writeFakeNMCLI(t,
		reply{args: "-t -f UUID,TYPE connection show", out: "uuid-a:802-11-wireless\n"},
		reply{args: profileArgs("uuid-a"), out: "Error: NetworkManager is not running.\n", code: 8},
	)
	if got, err := existingProfileUUID(context.Background(), path, "Home"); err == nil {
		t.Fatalf("existingProfileUUID = %q, nil; want an error", got)
	}
}

func TestListKnownProfilesWithSeveralProfiles(t *testing.T) {
	path, _ := writeFakeNMCLI(t, severalProfiles()...)
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
	got, err := n.listKnownProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []knownProfile{
		{Name: "Home", UUID: "uuid-a", Priority: 5, SSID: "Home", Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK},
		{Name: "Cafe", UUID: "uuid-b", Priority: 0, SSID: "Cafe", Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN},
		{Name: "Office", UUID: "uuid-c", Priority: 2, SSID: "Office", Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d profiles %+v; want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("profile %d = %+v; want %+v", i, got[i], want[i])
		}
	}
}

func TestListKnownProfilesFallsBackToName(t *testing.T) {
	path, _ := writeFakeNMCLI(t,
		reply{args: "-t -f NAME,UUID,TYPE,AUTOCONNECT-PRIORITY connection show", out: "Home:uuid-a:802-11-wireless:0\n"},
		reply{args: batchArgs("uuid-a"), out: "Error: timeout.\n", code: 1},
		reply{args: profileArgs("uuid-a"), out: "Error: timeout.\n", code: 1},
	)
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
	got, err := n.listKnownProfiles(context.Background())
	if err != nil || len(got) != 1 || got[0].SSID != "Home" {
		t.Fatalf("listKnownProfiles = %+v, %v; want the name as SSID", got, err)
	}
}
