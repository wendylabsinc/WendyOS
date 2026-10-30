package network

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeProfilesNMCLI stands in for nmcli on a device with saved profiles. Its
// state lives in files next to the script:
//
//   - profiles/<uuid>/: one directory per saved profile, one file per
//     property (name, type, priority, ssid, key-mgmt, psk, hidden). An open
//     network has no key-mgmt file, as NetworkManager stores it without a
//     security setting.
//   - scan: NetworkManager's list, `IN-USE:SSID:SIGNAL:SECURITY` rows.
//   - aps: one `ssid|key-mgmt|psk` line per combination an access point
//     accepts. Activating anything else fails the way NetworkManager reports
//     a bad or missing key: "Secrets were required, but not provided".
//   - error: while it exists, every activation fails with its content
//     instead.
//   - log: every command line it was run with.
//
// Like nmcli 1.56, `connection show` lists profiles by name, and `-g` prints
// one block per profile in argument order, with an empty line between blocks
// and nothing for a setting the profile doesn't have. `device wifi connect`
// reuses a saved profile for the SSID and writes the password into it, or
// adds one with the key-mgmt NetworkManager picks from the scan (wpa-psk
// for a WPA2/WPA3 transition network), and leaves it behind when activation
// fails.
const fakeProfilesNMCLI = `#!/bin/sh
cd "$(dirname "$0")" || exit 1
echo "$*" >> log

uuids() {
	for p in profiles/*; do
		[ -d "$p" ] || continue
		printf '%s\t%s\n' "$(cat "$p/name")" "${p#profiles/}"
	done | LC_ALL=C sort | cut -f2
}

set_prop() {
	case $2 in
	con-name) printf '%s\n' "$3" > "$1/name" ;;
	ssid) printf '%s\n' "$3" > "$1/ssid" ;;
	connection.autoconnect-priority) printf '%s\n' "$3" > "$1/priority" ;;
	802-11-wireless.hidden) printf '%s\n' "$3" > "$1/hidden" ;;
	802-11-wireless-security.key-mgmt) printf '%s\n' "$3" > "$1/key-mgmt" ;;
	802-11-wireless-security.psk) printf '%s\n' "$3" > "$1/psk" ;;
	esac
}

new_profile() {
	n=$(( $(cat added 2>/dev/null || echo 0) + 1 ))
	echo $n > added
	p=profiles/$(printf 'a0000000-0000-4000-8000-%012d' $n)
	mkdir -p "$p"
	echo 802-11-wireless > "$p/type"
	echo 0 > "$p/priority"
	echo "$p"
}

activate() {
	if [ -e error ]; then
		cat error >&2
		exit 4
	fi
	if grep -Fxq "$(cat "$1/ssid")|$(cat "$1/key-mgmt" 2>/dev/null)|$(cat "$1/psk" 2>/dev/null)" aps; then
		echo "Connection successfully activated (D-Bus active path: /org/freedesktop/NetworkManager/ActiveConnection/7)"
		exit 0
	fi
	echo "Error: Connection activation failed: Secrets were required, but not provided." >&2
	exit 4
}

need_profile() {
	if [ ! -d "profiles/$1" ]; then
		echo "Error: $1 - no such connection profile." >&2
		exit 10
	fi
}

if [ "$*" = "-t -f NAME,UUID,TYPE,AUTOCONNECT-PRIORITY connection show" ]; then
	for u in $(uuids); do
		echo "$(cat profiles/$u/name):$u:$(cat profiles/$u/type):$(cat profiles/$u/priority)"
	done
elif [ "$*" = "-t -f NAME,UUID,TYPE connection show" ]; then
	for u in $(uuids); do
		echo "$(cat profiles/$u/name):$u:$(cat profiles/$u/type)"
	done
elif [ "$1 $2 $4 $5" = "-t -g connection show" ]; then
	fields=$(echo "$3" | tr , ' ')
	shift 5
	first=yes
	for u in "$@"; do
		need_profile "$u"
		[ -n "$first" ] || echo
		first=
		for f in $fields; do
			case $f in
			connection.uuid) echo "$u" ;;
			802-11-wireless.ssid) cat "profiles/$u/ssid" ;;
			802-11-wireless-security.key-mgmt)
				if [ -e "profiles/$u/key-mgmt" ]; then cat "profiles/$u/key-mgmt"; fi ;;
			*) echo "fake nmcli: unexpected field $f" >&2; exit 2 ;;
			esac
		done
	done
elif [ "$1 $2" = "connection delete" ]; then
	need_profile "$3"
	rm -r "profiles/$3"
elif [ "$1 $2" = "connection modify" ]; then
	need_profile "$3"
	p=profiles/$3
	shift 3
	while [ $# -ge 2 ]; do set_prop "$p" "$1" "$2"; shift 2; done
elif [ "$1 $2" = "connection add" ]; then
	p=$(new_profile)
	shift 2
	while [ $# -ge 2 ]; do set_prop "$p" "$1" "$2"; shift 2; done
elif [ "$1 $2" = "connection up" ]; then
	need_profile "$3"
	activate "profiles/$3"
elif [ "$1 $2 $3" = "device wifi connect" ]; then
	ssid=$4
	shift 4
	password=
	while [ $# -ge 2 ]; do
		if [ "$1" = password ]; then password=$2; fi
		shift 2
	done
	if ! grep -qF ":$ssid:" scan; then
		echo "Error: No network with SSID '$ssid' found." >&2
		exit 10
	fi
	sec=$(grep -F ":$ssid:" scan | head -n 1 | cut -d: -f4)
	p=
	for u in $(uuids); do
		if [ "$(cat "profiles/$u/ssid" 2>/dev/null)" = "$ssid" ]; then
			p=profiles/$u
			break
		fi
	done
	if [ -z "$p" ]; then
		p=$(new_profile)
		set_prop "$p" con-name "$ssid"
		set_prop "$p" ssid "$ssid"
		case $sec in
		"") ;;
		*WPA1*|*WPA2*) set_prop "$p" 802-11-wireless-security.key-mgmt wpa-psk ;;
		*WPA3*) set_prop "$p" 802-11-wireless-security.key-mgmt sae ;;
		esac
	fi
	if [ -n "$password" ]; then set_prop "$p" 802-11-wireless-security.psk "$password"; fi
	activate "$p"
elif [ "$*" = "-t -f IN-USE,SSID,SIGNAL,SECURITY device wifi list --rescan no" ]; then
	cat scan
else
	echo "fake nmcli: unexpected command: $*" >&2
	exit 2
fi
`

// fakeProfile is one saved NetworkManager profile in fakeProfilesNMCLI.
type fakeProfile struct {
	UUID     string
	Name     string
	Type     string // "" means 802-11-wireless
	SSID     string
	KeyMgmt  string // "" means an open network
	PSK      string
	Priority int
	Hidden   bool
}

// savedProfiles is a device with five saved WiFi profiles and a wired one.
// nmcli lists them by name: Alpha, Bravo, Charlie, Delta Guest, Wired
// connection 1, preconfigured. Bravo is hidden, Delta Guest is open, and
// preconfigured (the name Raspberry Pi Imager gives the network it bakes
// into an image) holds HomeNet.
var savedProfiles = []fakeProfile{
	{UUID: "5b7e3a0c-1d2f-4e6a-9b8c-0d1e2f3a4b5c", Name: "Alpha", SSID: "Alpha", KeyMgmt: "wpa-psk", PSK: "alpha-pass", Priority: 10},
	{UUID: "8c2d4f6a-3b5e-4c7d-8e9f-1a2b3c4d5e6f", Name: "Bravo", SSID: "Bravo", KeyMgmt: "wpa-psk", PSK: "bravo-pass", Priority: 20, Hidden: true},
	{UUID: "1e3f5a7c-9b2d-4f6e-8a1c-3e5f7a9b2d4f", Name: "Charlie", SSID: "Charlie", KeyMgmt: "sae", PSK: "charlie-pass", Priority: 30},
	{UUID: "d4c3b2a1-6f5e-4d8c-9b0a-7e6f5d4c3b2a", Name: "Delta Guest", SSID: "Delta Guest"},
	{UUID: "f0e1d2c3-b4a5-4697-8879-6a5b4c3d2e1f", Name: "Wired connection 1", Type: "802-3-ethernet", Priority: -999},
	{UUID: "3a9f1c2e-7d4b-4e8a-b6c5-2f1e0d9c8b7a", Name: "preconfigured", SSID: "HomeNet", KeyMgmt: "wpa-psk", PSK: "home-pass", Priority: 50},
}

// uuidOf returns the UUID of the profile for ssid in savedProfiles.
func uuidOf(t *testing.T, ssid string) string {
	t.Helper()
	for _, p := range savedProfiles {
		if p.SSID == ssid {
			return p.UUID
		}
	}
	t.Fatalf("no saved profile for %q", ssid)
	return ""
}

type fakeProfiles struct {
	t       *testing.T
	dir     string
	manager *NMCLINetworkManager
}

// newFakeProfiles returns a manager running fakeProfilesNMCLI with
// savedProfiles saved, an empty scan, and no access point accepting
// anything.
func newFakeProfiles(t *testing.T) *fakeProfiles {
	t.Helper()
	f := &fakeProfiles{t: t, dir: t.TempDir()}
	f.write("nmcli", fakeProfilesNMCLI)
	f.write("scan", "")
	f.write("aps", "")
	for _, p := range savedProfiles {
		f.save(p)
	}
	f.manager = &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: filepath.Join(f.dir, "nmcli")}
	return f
}

func (f *fakeProfiles) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeProfiles) save(p fakeProfile) {
	f.t.Helper()
	typ := p.Type
	if typ == "" {
		typ = "802-11-wireless"
	}
	dir := filepath.Join("profiles", p.UUID)
	f.write(filepath.Join(dir, "name"), p.Name+"\n")
	f.write(filepath.Join(dir, "type"), typ+"\n")
	f.write(filepath.Join(dir, "priority"), strconv.Itoa(p.Priority)+"\n")
	if typ != "802-11-wireless" {
		return
	}
	f.write(filepath.Join(dir, "ssid"), p.SSID+"\n")
	if p.KeyMgmt != "" {
		f.write(filepath.Join(dir, "key-mgmt"), p.KeyMgmt+"\n")
		f.write(filepath.Join(dir, "psk"), p.PSK+"\n")
	}
	if p.Hidden {
		f.write(filepath.Join(dir, "hidden"), "yes\n")
	}
}

// setScan sets NetworkManager's list to the given `IN-USE:SSID:SIGNAL:SECURITY`
// rows.
func (f *fakeProfiles) setScan(rows ...string) {
	f.t.Helper()
	f.write("scan", strings.Join(rows, "\n")+"\n")
}

// accept makes the access points accept the given `ssid|key-mgmt|psk`
// combinations.
func (f *fakeProfiles) accept(combos ...string) {
	f.t.Helper()
	f.write("aps", strings.Join(combos, "\n")+"\n")
}

// profiles returns the saved profiles, keyed by UUID.
func (f *fakeProfiles) profiles() map[string]fakeProfile {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.dir, "profiles"))
	if err != nil {
		f.t.Fatal(err)
	}
	read := func(uuid, prop string) string {
		data, err := os.ReadFile(filepath.Join(f.dir, "profiles", uuid, prop))
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if err != nil {
			f.t.Fatal(err)
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	out := make(map[string]fakeProfile, len(entries))
	for _, e := range entries {
		u := e.Name()
		prio, _ := strconv.Atoi(read(u, "priority"))
		typ := read(u, "type")
		if typ == "802-11-wireless" {
			typ = ""
		}
		out[u] = fakeProfile{
			UUID:     u,
			Name:     read(u, "name"),
			Type:     typ,
			SSID:     read(u, "ssid"),
			KeyMgmt:  read(u, "key-mgmt"),
			PSK:      read(u, "psk"),
			Priority: prio,
			Hidden:   read(u, "hidden") == "yes",
		}
	}
	return out
}

// requireProfiles fails unless the saved profiles are savedProfiles with
// the given changes: a fakeProfile replaces or adds the profile with its
// UUID, and a bare UUID string removes one.
func (f *fakeProfiles) requireProfiles(changes ...any) {
	f.t.Helper()
	want := make(map[string]fakeProfile, len(savedProfiles))
	for _, p := range savedProfiles {
		want[p.UUID] = p
	}
	for _, c := range changes {
		switch c := c.(type) {
		case fakeProfile:
			want[c.UUID] = c
		case string:
			delete(want, c)
		default:
			f.t.Fatalf("requireProfiles: unexpected change %T", c)
		}
	}
	got := f.profiles()
	for u, w := range want {
		g, ok := got[u]
		if !ok {
			f.t.Errorf("profile %s (%s) was deleted", u, w.Name)
			continue
		}
		if g != w {
			f.t.Errorf("profile %s = %+v; want %+v", u, g, w)
		}
	}
	for u, g := range got {
		if _, ok := want[u]; !ok {
			f.t.Errorf("unexpected profile %s left behind: %+v", u, g)
		}
	}
}

// calls returns the nmcli command lines that change something or activate a
// profile, in order: add, modify, delete, up, and device wifi connect.
func (f *fakeProfiles) calls() []string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "log"))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		for _, verb := range []string{"connection add", "connection modify", "connection delete", "connection up", "device wifi connect"} {
			if strings.HasPrefix(line, verb+" ") {
				out = append(out, line)
			}
		}
	}
	return out
}

func (f *fakeProfiles) requireCalls(want ...string) {
	f.t.Helper()
	if got := f.calls(); !slices.Equal(got, want) {
		f.t.Fatalf("nmcli calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// profileDetailsOutput is what nmcli 1.56 prints for
//
//	nmcli -t -g connection.uuid,802-11-wireless.ssid,802-11-wireless-security.key-mgmt \
//	    connection show <the five WiFi UUIDs of savedProfiles, in list order>
//
// An empty line separates profiles. Delta Guest, an open network, has no
// security setting, so its block has no key-mgmt line. Bravo is hidden,
// which changes nothing here.
const profileDetailsOutput = `5b7e3a0c-1d2f-4e6a-9b8c-0d1e2f3a4b5c
Alpha
wpa-psk

8c2d4f6a-3b5e-4c7d-8e9f-1a2b3c4d5e6f
Bravo
wpa-psk

1e3f5a7c-9b2d-4f6e-8a1c-3e5f7a9b2d4f
Charlie
sae

d4c3b2a1-6f5e-4d8c-9b0a-7e6f5d4c3b2a
Delta Guest

3a9f1c2e-7d4b-4e8a-b6c5-2f1e0d9c8b7a
HomeNet
wpa-psk
`

// TestFakeNMCLIPrintsRealProfileDetails pins the fake to the real output
// format, so the tests below feed the agent what a device would.
func TestFakeNMCLIPrintsRealProfileDetails(t *testing.T) {
	f := newFakeProfiles(t)
	args := []string{"-t", "-g", "connection.uuid,802-11-wireless.ssid,802-11-wireless-security.key-mgmt", "connection", "show"}
	for _, ssid := range []string{"Alpha", "Bravo", "Charlie", "Delta Guest", "HomeNet"} {
		args = append(args, uuidOf(t, ssid))
	}
	out, err := exec.Command(f.manager.nmcliPath, args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != profileDetailsOutput {
		t.Fatalf("fake nmcli printed:\n%s\nwant:\n%s", out, profileDetailsOutput)
	}
}

// TestListKnownWiFiNetworksReadsEveryProfile: every saved profile gets its
// own SSID and security, however many profiles come before it and whether
// or not they have a security setting. Reading nmcli's output as two lines
// per profile gave the third profile on a device the SSID "wpa-psk" and
// security Open.
func TestListKnownWiFiNetworksReadsEveryProfile(t *testing.T) {
	f := newFakeProfiles(t)
	known, err := f.manager.ListKnownWiFiNetworks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		SSID     string
		UUID     string
		Priority int32
		Security agentpb.WiFiSecurityType
	}
	var got []entry
	for _, k := range known {
		got = append(got, entry{k.GetSsid(), k.GetUuid(), k.GetPriority(), k.GetSecurity()})
	}
	want := []entry{
		{"HomeNet", uuidOf(t, "HomeNet"), 50, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK},
		{"Charlie", uuidOf(t, "Charlie"), 30, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE},
		{"Bravo", uuidOf(t, "Bravo"), 20, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK},
		{"Alpha", uuidOf(t, "Alpha"), 10, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK},
		{"Delta Guest", uuidOf(t, "Delta Guest"), 0, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("known networks:\n  %+v\nwant:\n  %+v", got, want)
	}
}

// TestForgetWiFiNetworkDeletesItsOwnProfile: forgetting a network deletes
// that network's profile and no other.
func TestForgetWiFiNetworkDeletesItsOwnProfile(t *testing.T) {
	for _, ssid := range []string{"Alpha", "Bravo", "Charlie", "Delta Guest", "HomeNet"} {
		t.Run(ssid, func(t *testing.T) {
			f := newFakeProfiles(t)
			if err := f.manager.ForgetWiFiNetwork(context.Background(), ssid); err != nil {
				t.Fatal(err)
			}
			uuid := uuidOf(t, ssid)
			f.requireCalls("connection delete " + uuid)
			f.requireProfiles(uuid)
		})
	}
}

// TestReorderKnownWiFiNetworksSetsEachProfilesPriority: a reorder sets each
// network's own priority.
func TestReorderKnownWiFiNetworksSetsEachProfilesPriority(t *testing.T) {
	f := newFakeProfiles(t)
	order := []string{"Delta Guest", "HomeNet", "Charlie", "Bravo", "Alpha"}
	if err := f.manager.ReorderKnownWiFiNetworks(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var changes []any
	for i, ssid := range order {
		for _, p := range savedProfiles {
			if p.SSID == ssid {
				p.Priority = len(order) - i
				changes = append(changes, p)
			}
		}
	}
	f.requireProfiles(changes...)
}

// TestConnectToWiFiCleanupDeletesOnlyTheFailedProfile: a failed connect to
// a new network deletes the profile nmcli saved for it, and leaves every
// other saved network alone. Bistro is listed between Alpha and Bravo,
// which is where misreading nmcli's output pointed the cleanup at Bravo.
func TestConnectToWiFiCleanupDeletesOnlyTheFailedProfile(t *testing.T) {
	f := newFakeProfiles(t)
	f.setScan(":Bistro:70:WPA2", ":Bravo:60:WPA2")
	f.accept("Bistro|wpa-psk|right-pass")

	err := f.manager.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Bistro", Password: "wrong-pass"})
	if err == nil || !strings.Contains(err.Error(), "Secrets were required") {
		t.Fatalf("err = %v; want the activation failure", err)
	}
	const added = "a0000000-0000-4000-8000-000000000001"
	f.requireCalls(
		"device wifi connect Bistro password wrong-pass",
		"connection delete "+added,
	)
	f.requireProfiles()
}

// TestConnectToWiFiCleanupKeepsAProfileThatWasAlreadySaved: a failed
// connect to a saved network keeps its profile.
func TestConnectToWiFiCleanupKeepsAProfileThatWasAlreadySaved(t *testing.T) {
	f := newFakeProfiles(t)
	f.setScan(":Charlie:70:WPA3", ":HomeNet:60:WPA2")

	err := f.manager.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "HomeNet", Password: "typo"})
	if err == nil {
		t.Fatal("connect with a wrong password succeeded")
	}
	f.requireCalls("device wifi connect HomeNet password typo")
	home := savedProfiles[len(savedProfiles)-1]
	home.PSK = "typo" // nmcli writes the password into the profile it reuses
	f.requireProfiles(home)
}

// TestConnectToWiFiWithHintUsesTheRightProfile: a connect with a security
// hint updates and brings up the network's own saved profile.
func TestConnectToWiFiWithHintUsesTheRightProfile(t *testing.T) {
	f := newFakeProfiles(t)
	f.accept("Bravo|wpa-psk|new-pass")
	sec := agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK
	hidden := true

	err := f.manager.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{
		Ssid: "Bravo", Password: "new-pass", Security: &sec, Hidden: &hidden,
	})
	if err != nil {
		t.Fatal(err)
	}
	bravo := uuidOf(t, "Bravo")
	f.requireCalls(
		"connection modify "+bravo+" 802-11-wireless.hidden yes 802-11-wireless-security.key-mgmt wpa-psk 802-11-wireless-security.psk new-pass",
		"connection up "+bravo,
	)
	updated := savedProfiles[1]
	updated.PSK = "new-pass"
	f.requireProfiles(updated)
}

// TestExistingProfileUUID: every saved SSID maps to its own profile, and an
// SSID with no profile maps to none.
func TestExistingProfileUUID(t *testing.T) {
	f := newFakeProfiles(t)
	var ssids []string
	for _, p := range savedProfiles {
		if p.SSID != "" {
			ssids = append(ssids, p.SSID)
		}
	}
	for _, ssid := range append(ssids, "Bistro") {
		got, err := existingProfileUUID(context.Background(), f.manager.nmcliPath, ssid)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if ssid != "Bistro" {
			want = uuidOf(t, ssid)
		}
		if got != want {
			t.Errorf("existingProfileUUID(%q) = %q; want %q", ssid, got, want)
		}
	}
}

// transitionSSID is a WPA2/WPA3 transition network that refuses wpa-psk
// from this radio, like the one that answered "Secrets were required, but
// not provided" until it was joined with a WPA3-SAE hint.
const transitionSSID = "TMNL-E2B01D"

// newTransitionNetwork returns a fake with transitionSSID, a WPA2-only
// network (Bistro) and a WPA3-only one (Lab6E) in range. Each accepts
// right-pass, and transitionSSID only over sae.
func newTransitionNetwork(t *testing.T) *fakeProfiles {
	t.Helper()
	f := newFakeProfiles(t)
	f.setScan(":"+transitionSSID+":72:WPA2 WPA3", ":Bistro:70:WPA2", ":Lab6E:65:WPA3")
	f.accept(transitionSSID+"|sae|right-pass", "Bistro|wpa-psk|right-pass", "Lab6E|sae|right-pass")
	return f
}

// UUIDs the fake gives the first and second profiles a test adds.
const (
	firstAdded  = "a0000000-0000-4000-8000-000000000001"
	secondAdded = "a0000000-0000-4000-8000-000000000002"
)

// TestConnectToWiFiRetriesTransitionNetworkAsSAE: NetworkManager joins a
// WPA2/WPA3 transition network with wpa-psk when given no hint. When that
// fails for missing secrets, the agent deletes the profile it left and
// retries once with an sae profile.
func TestConnectToWiFiRetriesTransitionNetworkAsSAE(t *testing.T) {
	f := newTransitionNetwork(t)
	err := f.manager.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: transitionSSID, Password: "right-pass"})
	if err != nil {
		t.Fatal(err)
	}
	f.requireCalls(
		"device wifi connect "+transitionSSID+" password right-pass",
		"connection delete "+firstAdded,
		"connection add type wifi con-name "+transitionSSID+" ssid "+transitionSSID+" autoconnect yes 802-11-wireless-security.key-mgmt sae 802-11-wireless-security.psk right-pass",
		"connection up "+secondAdded,
	)
	f.requireProfiles(fakeProfile{UUID: secondAdded, Name: transitionSSID, SSID: transitionSSID, KeyMgmt: "sae", PSK: "right-pass"})
}

// TestConnectToWiFiSAERetryCleansUpWhenBothAttemptsFail: when the sae retry
// fails too, neither attempt leaves a profile behind, every other saved
// network stays, and the error reports both attempts.
func TestConnectToWiFiSAERetryCleansUpWhenBothAttemptsFail(t *testing.T) {
	f := newTransitionNetwork(t)
	err := f.manager.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: transitionSSID, Password: "wrong-pass"})
	if err == nil {
		t.Fatal("connect with a wrong password succeeded")
	}
	if !strings.Contains(err.Error(), "nmcli connect: Error: Connection activation failed: Secrets were required") ||
		!strings.Contains(err.Error(), "retrying as WPA3-SAE also failed: nmcli connection up "+transitionSSID) {
		t.Errorf("err = %v; want both attempts' failures", err)
	}
	f.requireCalls(
		"device wifi connect "+transitionSSID+" password wrong-pass",
		"connection delete "+firstAdded,
		"connection add type wifi con-name "+transitionSSID+" ssid "+transitionSSID+" autoconnect yes 802-11-wireless-security.key-mgmt sae 802-11-wireless-security.psk wrong-pass",
		"connection up "+secondAdded,
		"connection delete "+secondAdded,
	)
	f.requireProfiles()
}

// TestConnectToWiFiDoesNotRetryAsSAE: every other connect without a hint
// behaves as before: one attempt, and its own error.
func TestConnectToWiFiDoesNotRetryAsSAE(t *testing.T) {
	savedTransition := fakeProfile{
		UUID: "7c1b9e2d-4a6f-4b8c-9d0e-1f2a3b4c5d6e", Name: transitionSSID, SSID: transitionSSID,
		KeyMgmt: "wpa-psk", PSK: "old-pass", Priority: 5,
	}
	reused := savedTransition
	reused.PSK = "right-pass" // nmcli writes the password into the profile it reuses
	cases := []struct {
		name     string
		ssid     string
		password string
		setup    func(f *fakeProfiles)
		wantErr  bool
		calls    []string
		changes  []any
	}{
		{
			name: "WPA2-only network, wrong password", ssid: "Bistro", password: "wrong-pass", wantErr: true,
			calls: []string{"device wifi connect Bistro password wrong-pass", "connection delete " + firstAdded},
		},
		{
			name: "WPA2-only network, right password", ssid: "Bistro", password: "right-pass",
			calls:   []string{"device wifi connect Bistro password right-pass"},
			changes: []any{fakeProfile{UUID: firstAdded, Name: "Bistro", SSID: "Bistro", KeyMgmt: "wpa-psk", PSK: "right-pass"}},
		},
		{
			name: "WPA3-only network, wrong password", ssid: "Lab6E", password: "wrong-pass", wantErr: true,
			calls: []string{"device wifi connect Lab6E password wrong-pass", "connection delete " + firstAdded},
		},
		{
			name: "transition network, failure other than missing secrets", ssid: transitionSSID, password: "right-pass", wantErr: true,
			setup: func(f *fakeProfiles) {
				f.write("error", "Error: Connection activation failed: IP configuration could not be reserved (no available address, timeout, etc.).\n")
			},
			calls: []string{"device wifi connect " + transitionSSID + " password right-pass", "connection delete " + firstAdded},
		},
		{
			name: "transition network, profile already saved", ssid: transitionSSID, password: "right-pass", wantErr: true,
			setup:   func(f *fakeProfiles) { f.save(savedTransition) },
			calls:   []string{"device wifi connect " + transitionSSID + " password right-pass"},
			changes: []any{reused},
		},
		{
			name: "transition network, no password", ssid: transitionSSID, wantErr: true,
			calls: []string{"device wifi connect " + transitionSSID, "connection delete " + firstAdded},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTransitionNetwork(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			err := f.manager.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: tc.ssid, Password: tc.password})
			if tc.wantErr {
				if err == nil {
					t.Fatal("connect succeeded")
				}
				if strings.Contains(err.Error(), "WPA3-SAE") {
					t.Errorf("err = %v; want only the first attempt's error", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			f.requireCalls(tc.calls...)
			f.requireProfiles(tc.changes...)
		})
	}
}
