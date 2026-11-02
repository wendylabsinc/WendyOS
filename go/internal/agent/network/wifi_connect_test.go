package network

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const listUUIDs = "-t -f UUID,TYPE connection show"

// callsWithPrefix returns the logged calls that start with prefix.
func callsWithPrefix(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

const newUUID = "0f9e8d7c-6b5a-4c3d-2e1f-001122334455"

// addOK is nmcli's answer to a successful `connection add`.
var addOK = reply{args: "connection add ", prefix: true,
	out: "Connection 'Hidden' (" + newUUID + ") successfully added.\n"}

func connect(t *testing.T, path string, req *agentpb.ConnectToWiFiRequest) error {
	t.Helper()
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
	return n.ConnectToWiFi(context.Background(), req)
}

func TestAddedUUIDRe(t *testing.T) {
	cases := map[string]string{
		"Connection 'Home' (" + newUUID + ") successfully added.\n":                                                        newUUID,
		"Connection 'a (b) c' (" + newUUID + ") successfully added.\n":                                                     newUUID,
		"Connection 'x (11111111-2222-3333-4444-555555555555) successfully added' (" + newUUID + ") successfully added.\n": newUUID,
		"Warning: something.\n": "",
	}
	for out, want := range cases {
		got := ""
		if m := addedUUIDRe.FindAllStringSubmatch(out, -1); len(m) > 0 {
			got = m[len(m)-1][1]
		}
		if got != want {
			t.Errorf("uuid in %q = %q; want %q", out, got, want)
		}
	}
}

func TestConnectNewProfileLooksUpOnce(t *testing.T) {
	path, log := writeFakeNMCLI(t, append(severalProfiles(),
		addOK,
		reply{args: "connection up ", prefix: true},
	)...)
	if err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: "Hidden", Password: "secret123", Hidden: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, log)
	if got := len(callsWithPrefix(calls, listUUIDs)); got != 1 {
		t.Errorf("profiles listed %d times; want 1:\n%s", got, strings.Join(calls, "\n"))
	}
	adds := callsWithPrefix(calls, "connection add ")
	if len(adds) != 1 || strings.Contains(adds[0], "connection.uuid") {
		t.Fatalf("adds %q; want one, without connection.uuid", adds)
	}
	if ups := callsWithPrefix(calls, "connection up "); len(ups) != 1 || ups[0] != "connection up "+newUUID {
		t.Errorf("activated %q; want the added profile", ups)
	}
}

func TestConnectFailedNewProfileIsDeleted(t *testing.T) {
	path, log := writeFakeNMCLI(t, append(severalProfiles(),
		addOK,
		reply{args: "connection up ", prefix: true, out: "Error: Secrets were required.\n", code: 4},
		reply{args: "connection delete ", prefix: true},
	)...)
	if err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: "Hidden", Password: "wrong-pass", Hidden: proto.Bool(true)}); err == nil {
		t.Fatal("want the activation error")
	}
	calls := readCalls(t, log)
	if dels := callsWithPrefix(calls, "connection delete "); len(dels) != 1 || dels[0] != "connection delete "+newUUID {
		t.Errorf("deleted %q; want only the profile this attempt added", dels)
	}
	if got := len(callsWithPrefix(calls, listUUIDs)); got != 1 {
		t.Errorf("profiles listed %d times; want 1", got)
	}
}

// Without a UUID in nmcli's answer, the added profile is found by its SSID.
func TestConnectFindsAddedProfileWithoutUUIDLine(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nmcli"
	script := `#!/bin/sh
d=$(dirname "$0")
case "$*" in
"` + listUUIDs + `") [ -f "$d/added" ] && echo "uuid-x:802-11-wireless"; exit 0 ;;
"` + batchArgs("uuid-x") + `") printf '%s' '` + batchRecord("uuid-x", "wpa-psk", "", "Hidden") + `'; exit 0 ;;
"connection add "*) touch "$d/added"; exit 0 ;;
"connection up uuid-x") exit 0 ;;
esac
echo "unexpected nmcli call: $*" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: "Hidden", Password: "secret123", Hidden: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectFailedExistingProfileIsKept(t *testing.T) {
	path, log := writeFakeNMCLI(t, append(severalProfiles(),
		reply{args: "connection modify uuid-c ", prefix: true},
		reply{args: "connection up uuid-c", out: "Error: Secrets were required.\n", code: 4},
	)...)
	if err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: "Office", Password: "wrong-pass", Hidden: proto.Bool(true)}); err == nil {
		t.Fatal("want the activation error")
	}
	calls := readCalls(t, log)
	if dels := callsWithPrefix(calls, "connection delete "); len(dels) != 0 {
		t.Errorf("deleted %q; a profile that existed before must be kept", dels)
	}
	if adds := callsWithPrefix(calls, "connection add "); len(adds) != 0 {
		t.Errorf("added %q; the existing profile must be reused", adds)
	}
}

const (
	scanArgs   = "-t -f SSID,SSID-HEX,SECURITY,DEVICE device wifi list --rescan no"
	activeArgs = "-t -f UUID,DEVICE connection show --active"
)

// scanRow renders one BSS seen on wlan0 the way the terse scan list prints it.
func scanRow(ssid, security string) string {
	return scanRowOn("wlan0", ssid, security)
}

// scanRowOn is scanRow for a BSS seen on device.
func scanRowOn(device, ssid, security string) string {
	escaped := strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(ssid)
	return escaped + ":" + strings.ToUpper(hex.EncodeToString([]byte(ssid))) + ":" + security + ":" + device + "\n"
}

// connectVisible connects to ssid with the given saved profiles and scan
// result, and reports the nmcli calls that changed or activated a profile.
func connectVisible(t *testing.T, profiles []reply, ssid, password, scan string, upCode int) ([]string, error) {
	t.Helper()
	path, log := writeFakeNMCLI(t, append(profiles,
		reply{args: scanArgs, out: scan},
		reply{args: activeArgs},
		addOK,
		reply{args: "connection modify ", prefix: true},
		reply{args: "connection up ", prefix: true, code: upCode},
		reply{args: "connection delete ", prefix: true},
		reply{args: "device wifi connect ", prefix: true},
	)...)
	err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: ssid, Password: password})
	var changes []string
	for _, c := range readCalls(t, log) {
		if strings.HasPrefix(c, "connection ") && !strings.HasPrefix(c, "connection show") ||
			strings.HasPrefix(c, "device wifi connect") {
			changes = append(changes, c)
		}
	}
	return changes, err
}

// connectOK is connectVisible with severalProfiles and a successful activation.
func connectOK(t *testing.T, ssid, password, scan string) []string {
	t.Helper()
	changes, err := connectVisible(t, severalProfiles(), ssid, password, scan, 0)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestConnectRoutesPassphraseNetworksThroughProfile(t *testing.T) {
	cases := []struct {
		name, ssid, scan, keyMgmt string
	}{
		{"wpa2/wpa3 mixed", "Lab", scanRow("Lab", "WPA2 WPA3") + scanRow("Other", "WPA2"), "wpa-psk"},
		{"wpa2 only", "Lab", scanRow("Lab", "WPA1 WPA2"), "wpa-psk"},
		{"wpa3 only", "Lab", scanRow("Lab", "WPA3") + scanRow("Lab", "WPA3"), "sae"},
		{"wpa2 and wpa3-only bands", "Lab", scanRow("Lab", "WPA3") + scanRow("Lab", "WPA2"), "wpa-psk"},
		{"ssid with colon and backslash", `Lab:5G\x`, scanRow("Lab", "WPA2") + scanRow(`Lab:5G\x`, "WPA2 WPA3"), "wpa-psk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changes := connectOK(t, c.ssid, "secret123", c.scan)
			if len(changes) != 2 || !strings.HasPrefix(changes[0], "connection add ") || changes[1] != "connection up "+newUUID+" ifname wlan0" {
				t.Fatalf("calls %q; want connection add then up on wlan0", changes)
			}
			if !strings.Contains(changes[0], "802-11-wireless-security.key-mgmt "+c.keyMgmt+" ") {
				t.Errorf("add %q; want key-mgmt %s", changes[0], c.keyMgmt)
			}
			if strings.Contains(changes[0], authAlgField) {
				t.Errorf("add %q must leave auth-alg unset", changes[0])
			}
		})
	}
}

func TestConnectLeavesOtherNetworksToNMCLI(t *testing.T) {
	cases := []struct {
		name, password, scan string
	}{
		{"open network", "", scanRow("Lab", "")},
		{"open network with a typed password", "secret123", scanRow("Lab", "")},
		{"enhanced open", "secret123", scanRow("Lab", "OWE")},
		{"wep", "abcde", scanRow("Lab", "WEP")},
		{"enterprise", "secret123", scanRow("Lab", "WPA2 802.1X")},
		{"not in range", "secret123", scanRow("Other", "WPA2")},
		{"network without a password", "", scanRow("Lab", "WPA2")},
		// nmcli shows a non-UTF-8 SSID as text that differs from its raw bytes.
		{"ssid bytes differ from its text", "secret123", "Lab:4C61E2:WPA2:wlan0\n"},
		{"wep on one device", "secret123", scanRowOn("wlan0", "Lab", "WEP") + scanRowOn("wlan1", "Lab", "WPA2")},
		{"enterprise on one device", "secret123", scanRowOn("wlan0", "Lab", "WPA2") + scanRowOn("wlan1", "Lab", "WPA2 802.1X")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changes := connectOK(t, "Lab", c.password, c.scan)
			if len(changes) != 1 || !strings.HasPrefix(changes[0], "device wifi connect Lab") {
				t.Fatalf("calls %q; want only `device wifi connect`", changes)
			}
		})
	}
	t.Run("saved network without a password", func(t *testing.T) {
		if changes := connectOK(t, "Home", "", scanRow("Home", "WPA2")); len(changes) != 1 || changes[0] != "device wifi connect Home" {
			t.Fatalf("calls %q; want only `device wifi connect`", changes)
		}
	})
}

// reuseCalls is how a saved profile is updated and brought up on device.
func reuseCalls(uuid, keyMgmt, device string) []string {
	return []string{
		"connection modify " + uuid + " 802-11-wireless-security.key-mgmt " + keyMgmt +
			" 802-11-wireless-security.psk secret123 " + authAlgField + " ",
		"connection up " + uuid + " ifname " + device,
	}
}

func checkCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A saved profile the network accepts keeps its own key-mgmt, as with nmcli.
func TestConnectReusesAcceptedProfile(t *testing.T) {
	cases := []struct {
		name, ssid, scan, uuid, keyMgmt string
	}{
		{"wpa3 profile, mixed network", "Office", scanRow("Office", "WPA2 WPA3"), "uuid-c", "sae"},
		{"wpa3 profile, wpa3-only network", "Office", scanRow("Office", "WPA3"), "uuid-c", "sae"},
		{"wpa2 profile, mixed network", "Home", scanRow("Home", "WPA2 WPA3"), "uuid-a", "wpa-psk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkCalls(t, connectOK(t, c.ssid, "secret123", c.scan), reuseCalls(c.uuid, c.keyMgmt, "wlan0"))
		})
	}
}

// A saved profile the network can't use stays as it is, and a new one is
// added beside it, as with nmcli.
func TestConnectAddsProfileBesideUnusableOne(t *testing.T) {
	cases := []struct {
		name, ssid, scan, keyMgmt string
	}{
		{"open profile, wpa2 network", "Cafe", scanRow("Cafe", "WPA2"), "wpa-psk"},
		{"wpa2 profile, wpa3-only network", "Home", scanRow("Home", "WPA3"), "sae"},
		{"wpa3 profile, wpa2-only network", "Office", scanRow("Office", "WPA2"), "wpa-psk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changes := connectOK(t, c.ssid, "secret123", c.scan)
			if len(changes) != 2 || !strings.HasPrefix(changes[0], "connection add ") ||
				!strings.Contains(changes[0], "802-11-wireless-security.key-mgmt "+c.keyMgmt+" ") ||
				changes[1] != "connection up "+newUUID+" ifname wlan0" {
				t.Errorf("calls %q; want a new %s profile brought up on wlan0", changes, c.keyMgmt)
			}
		})
	}
}

// A WPA profile in use on the network is reused even when the cached scan
// misses the BSSes it suits; an open one is still left as it is.
func TestConnectReusesActiveProfile(t *testing.T) {
	withActive := func(active string) []reply {
		return append([]reply{{args: activeArgs, out: active}}, severalProfiles()...)
	}
	t.Run("wpa2 profile, cache shows only wpa3", func(t *testing.T) {
		changes, err := connectVisible(t, withActive("uuid-x:wlan0\nuuid-a:wlan1\n"), "Home", "secret123",
			scanRowOn("wlan0", "Home", "WPA3"), 0)
		if err != nil {
			t.Fatal(err)
		}
		checkCalls(t, changes, reuseCalls("uuid-a", "wpa-psk", "wlan1"))
	})
	t.Run("before an inactive profile the scan accepts", func(t *testing.T) {
		saved := append([]reply{{args: activeArgs, out: "uuid-a:wlan0\n"}}, savedFixture(
			savedRecord{"uuid-s", profileOut("sae", "", "Home")},
			savedRecord{"uuid-a", profileOut("wpa-psk", "", "Home")})...)
		changes, err := connectVisible(t, saved, "Home", "secret123", scanRow("Home", "WPA3"), 0)
		if err != nil {
			t.Fatal(err)
		}
		checkCalls(t, changes, reuseCalls("uuid-a", "wpa-psk", "wlan0"))
	})
	t.Run("open profile", func(t *testing.T) {
		changes, err := connectVisible(t, withActive("uuid-b:wlan0\n"), "Cafe", "secret123",
			scanRow("Cafe", "WPA2"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 2 || !strings.HasPrefix(changes[0], "connection add ") {
			t.Errorf("calls %q; want a new profile beside the open one", changes)
		}
	})
}

func TestConnectFailureDeletesOnlyAddedProfile(t *testing.T) {
	cases := []struct {
		name, ssid, scan string
		deleted          bool
	}{
		{"reused profile", "Office", scanRow("Office", "WPA3"), false},
		{"added beside an unusable profile", "Cafe", scanRow("Cafe", "WPA2"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changes, err := connectVisible(t, severalProfiles(), c.ssid, "secret123", c.scan, 4)
			if err == nil {
				t.Fatal("want the activation error")
			}
			var dels []string
			if c.deleted {
				dels = []string{"connection delete " + newUUID}
			}
			if got := callsWithPrefix(changes, "connection delete "); strings.Join(got, "\n") != strings.Join(dels, "\n") {
				t.Errorf("deleted %q; want %q", got, dels)
			}
			if mods := callsWithPrefix(changes, "connection modify uuid-b"); len(mods) != 0 {
				t.Errorf("modified the unusable profile: %q", mods)
			}
		})
	}
}

// savedRecord is one saved WiFi profile: its UUID and nmcli's output for it.
type savedRecord struct{ uuid, out string }

// savedFixture answers the profile list and the batched read for records.
func savedFixture(records ...savedRecord) []reply {
	list, uuids, outs := "", []string{}, []string{}
	for _, r := range records {
		list += r.uuid + ":802-11-wireless\n"
		uuids = append(uuids, r.uuid)
		outs = append(outs, uuidField+":"+r.uuid+"\n"+r.out)
	}
	return []reply{{args: listUUIDs, out: list}, {args: batchArgs(uuids...), out: batchOut(outs...)}}
}

// boundProfile is a saved profile for ssid bound to the device iface.
func boundProfile(iface, keyMgmt, ssid string) []reply {
	return savedFixture(savedRecord{"uuid-w", boundProfileOut(iface, keyMgmt, "open", ssid)})
}

func TestConnectReusesFirstAcceptedProfile(t *testing.T) {
	saved := savedFixture(
		savedRecord{"uuid-1", profileOut("", "", "Cafe")},
		savedRecord{"uuid-2", profileOut("wpa-psk", "", "Cafe")},
		savedRecord{"uuid-3", profileOut("wpa-psk", "", "Cafe")},
	)
	changes, err := connectVisible(t, saved, "Cafe", "secret123", scanRow("Cafe", "WPA2"), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkCalls(t, changes, reuseCalls("uuid-2", "wpa-psk", "wlan0"))
}

// nmcli prints an empty mode for profiles saved without one; they are clients.
func TestConnectReusesProfileWithoutMode(t *testing.T) {
	unset := strings.Replace(profileOut("wpa-psk", "", "Home"), modeField+":infrastructure", modeField+":", 1)
	changes, err := connectVisible(t, savedFixture(savedRecord{"uuid-m", unset}), "Home", "secret123", scanRow("Home", "WPA2"), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkCalls(t, changes, reuseCalls("uuid-m", "wpa-psk", "wlan0"))
}

// A hotspot profile for the SSID is never reused to join the network.
func TestConnectSkipsHotspotProfile(t *testing.T) {
	hotspot := strings.Replace(profileOut("wpa-psk", "", "Home"), modeField+":infrastructure", modeField+":ap", 1)
	for name, active := range map[string]string{"inactive": "", "active": "uuid-h:wlan0\n"} {
		t.Run(name, func(t *testing.T) {
			saved := append([]reply{{args: activeArgs, out: active}}, savedFixture(savedRecord{"uuid-h", hotspot})...)
			changes, err := connectVisible(t, saved, "Home", "secret123", scanRow("Home", "WPA2"), 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 2 || !strings.HasPrefix(changes[0], "connection add ") ||
				changes[1] != "connection up "+newUUID+" ifname wlan0" {
				t.Errorf("calls %q; want a new profile beside the hotspot one", changes)
			}
		})
	}
}

// The add reply is read untranslated whatever language the host uses.
func TestConnectReadsAddReplyUntranslated(t *testing.T) {
	t.Setenv("LANGUAGE", "de")
	path, _ := writeNMCLIScript(t, `"`+listUUIDs+`") echo "uuid-b:802-11-wireless"; exit 0 ;;
"`+batchArgs("uuid-b")+`") printf '%s' '`+batchRecord("uuid-b", "", "", "Cafe")+`'; exit 0 ;;
"`+scanArgs+`") printf '%s' '`+scanRow("Cafe", "WPA2")+`'; exit 0 ;;
"`+activeArgs+`") exit 0 ;;
"connection add "*) [ -n "$LANGUAGE" ] && { echo "Verbindung erfolgreich hinzugefuegt."; exit 0; }
	echo "Connection 'Cafe' (`+newUUID+`) successfully added."; exit 0 ;;
"connection up `+newUUID+` ifname wlan0") exit 0 ;;
`)
	if err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: "Cafe", Password: "secret123"}); err != nil {
		t.Fatal(err)
	}
}

// Without a UUID in the reply, a profile added beside another for the SSID
// can't be told apart, so neither is brought up or deleted.
func TestConnectStopsOnAmbiguousAddedProfile(t *testing.T) {
	path, log := writeNMCLIScript(t, `"`+listUUIDs+`") echo "uuid-b:802-11-wireless"; [ -f "$d/added" ] && echo "uuid-x:802-11-wireless"; exit 0 ;;
"`+batchArgs("uuid-b")+`") printf '%s' '`+batchRecord("uuid-b", "", "", "Cafe")+`'; exit 0 ;;
"`+batchArgs("uuid-b", "uuid-x")+`") printf '%s' '`+batchOut(batchRecord("uuid-b", "", "", "Cafe"), batchRecord("uuid-x", "wpa-psk", "", "Cafe"))+`'; exit 0 ;;
"`+scanArgs+`") printf '%s' '`+scanRow("Cafe", "WPA2")+`'; exit 0 ;;
"`+activeArgs+`") exit 0 ;;
"connection add "*) touch "$d/added"; exit 0 ;;
`)
	if err := connect(t, path, &agentpb.ConnectToWiFiRequest{Ssid: "Cafe", Password: "secret123"}); err == nil {
		t.Fatal("want an error")
	}
	calls := readCalls(t, log)
	if acts := append(callsWithPrefix(calls, "connection up "), callsWithPrefix(calls, "connection delete ")...); len(acts) != 0 {
		t.Errorf("calls %q; want no profile brought up or deleted", acts)
	}
}

// Like nmcli, a connect runs on a device that sees the network.
func TestConnectUsesDeviceThatSeesNetwork(t *testing.T) {
	t.Run("new profile", func(t *testing.T) {
		changes := connectOK(t, "Lab", "secret123", scanRowOn("wlan0", "Other", "WPA2")+
			scanRowOn("wlan1", "Lab", "WPA2")+scanRowOn("wlan2", "Lab", "WPA2"))
		if len(changes) != 2 || changes[1] != "connection up "+newUUID+" ifname wlan1" {
			t.Errorf("calls %q; want the new profile up on wlan1", changes)
		}
	})
	t.Run("profile bound to a device that sees it", func(t *testing.T) {
		changes, err := connectVisible(t, boundProfile("wlan1", "wpa-psk", "Lab"), "Lab", "secret123",
			scanRowOn("wlan0", "Lab", "WPA2")+scanRowOn("wlan1", "Lab", "WPA2"), 0)
		if err != nil {
			t.Fatal(err)
		}
		checkCalls(t, changes, reuseCalls("uuid-w", "wpa-psk", "wlan1"))
	})
	t.Run("profile bound to a device that doesn't", func(t *testing.T) {
		changes, err := connectVisible(t, boundProfile("wlan1", "wpa-psk", "Lab"), "Lab", "secret123",
			scanRowOn("wlan0", "Lab", "WPA2"), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 2 || !strings.HasPrefix(changes[0], "connection add ") ||
			changes[1] != "connection up "+newUUID+" ifname wlan0" {
			t.Errorf("calls %q; want a new profile up on wlan0", changes)
		}
	})
	// The profile is checked against what its own device sees.
	for name, c := range map[string]struct {
		saved         []reply
		scan, keyMgmt string
	}{
		"profile the first device's BSSes don't take": {
			savedFixture(savedRecord{"uuid-s", profileOut("sae", "", "Lab")}),
			scanRowOn("wlan0", "Lab", "WPA2") + scanRowOn("wlan1", "Lab", "WPA3"), "wpa-psk"},
		"bound profile its device's BSSes don't take": {
			boundProfile("wlan1", "sae", "Lab"),
			scanRowOn("wlan0", "Lab", "WPA3") + scanRowOn("wlan1", "Lab", "WPA2"), "sae"},
	} {
		t.Run(name, func(t *testing.T) {
			changes, err := connectVisible(t, c.saved, "Lab", "secret123", c.scan, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 2 || !strings.Contains(changes[0], "802-11-wireless-security.key-mgmt "+c.keyMgmt+" ") ||
				changes[1] != "connection up "+newUUID+" ifname wlan0" {
				t.Errorf("calls %q; want a new %s profile up on wlan0", changes, c.keyMgmt)
			}
		})
	}
}

func TestPassphraseSecurity(t *testing.T) {
	type flags struct{ psk, sae, ok bool }
	cases := map[string]flags{
		"WPA2":            {psk: true, ok: true},
		"WPA2 WPA3":       {psk: true, sae: true, ok: true},
		"WPA1":            {psk: true, ok: true},
		"WPA3":            {sae: true, ok: true},
		"":                {},
		"--":              {},
		"WEP":             {},
		"OWE":             {},
		"OWE-TM":          {},
		"WPA2 802.1X":     {},
		"WPA3 802.1X":     {},
		"WPA2 WPA3 OWE-X": {},
	}
	for security, want := range cases {
		psk, sae, ok := passphraseSecurity([]string{security})
		if got := (flags{psk, sae, ok}); got != want {
			t.Errorf("passphraseSecurity(%q) = %+v; want %+v", security, got, want)
		}
	}
	if psk, sae, ok := passphraseSecurity([]string{"WPA3", "", "WPA2"}); !psk || !sae || !ok {
		t.Errorf("BSSes with WPA3, none and WPA2 = %v %v %v; want both, ok", psk, sae, ok)
	}
	if _, _, ok := passphraseSecurity([]string{"WPA2", "WEP"}); ok {
		t.Error("a WEP BSS beside a WPA2 one must leave the network to nmcli")
	}
	if _, _, ok := passphraseSecurity(nil); ok {
		t.Error("no scan rows must not be a passphrase network")
	}
}
