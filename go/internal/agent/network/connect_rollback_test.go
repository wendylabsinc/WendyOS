package network

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeNMCLIScript models one access point and NetworkManager's saved profiles,
// with real nmcli 1.56 output: the active profile is listed first and values are
// printed raw under --escape no. Activation succeeds only when the psk matches the
// AP password, and `device wifi connect` saves the new password into a matching
// profile before trying it.
const fakeNMCLIScript = `#!/bin/sh
S=$FAKE_NMCLI_STATE
echo "$*" >> "$S/log"
get() { cat "$S/p/$1/$2" 2>/dev/null; }
put() { printf '%s' "$3" > "$S/p/$1/$2"; }
up() {
	if [ "$(get "$1" psk)" = "$(cat "$S/ap")" ]; then printf '%s' "$1" > "$S/active"; exit 0; fi
	: > "$S/active"
	echo "Error: Connection activation failed: Secrets were required, but not provided." >&2
	exit 4
}
profiles() {
	a=$(cat "$S/active"); [ -n "$a" ] && echo "$a"
	for d in "$S"/p/*; do [ -d "$d" ] && [ "${d##*/}" != "$a" ] && echo "${d##*/}"; done
}
[ "$1" = --wait ] && shift 2
case "$*" in
"-t -f UUID,TYPE connection show")
	for u in $(profiles); do printf '%s:802-11-wireless\n' "$u"; done
	exit 0;;
"-t -f UUID,TYPE connection show --active")
	a=$(cat "$S/active"); [ -n "$a" ] && printf '%s:802-11-wireless\n' "$a"
	exit 0;;
"--escape no -g 802-11-wireless.ssid connection show "*)
	get "$7" ssid; echo
	exit 0;;
"-s --escape no -g 802-11-wireless.hidden,802-11-wireless-security.key-mgmt,802-11-wireless-security.psk connection show "*)
	get "$8" hidden; echo
	[ -f "$S/p/$8/key-mgmt" ] && { get "$8" key-mgmt; echo; get "$8" psk; echo; }
	exit 0;;
esac
case "$1 $2" in
"device wifi")
	ssid=$4; shift 4; u=
	[ "$ssid" = Missing ] && { echo "Error: No network with SSID 'Missing' found." >&2; exit 10; }
	for d in "$S"/p/*; do
		[ -d "$d" ] && [ "$(get "${d##*/}" ssid)" = "$ssid" ] && [ -n "$(get "${d##*/}" key-mgmt)" ] && u=${d##*/} && break
	done
	if [ -z "$u" ]; then u=new; mkdir -p "$S/p/new"; put new ssid "$ssid"; put new hidden no; put new key-mgmt wpa-psk; fi
	while [ $# -gt 1 ]; do [ "$1" = password ] && put "$u" psk "$2"; shift 2; done
	up "$u";;
"connection add")
	shift 2; mkdir -p "$S/p/new"; put new hidden no
	while [ $# -gt 1 ]; do
		case "$1" in
		ssid) put new ssid "$2";;
		type|con-name|autoconnect) ;;
		*) put new "${1#*.}" "$2";;
		esac
		shift 2
	done;;
"connection modify")
	u=$3; shift 3
	while [ $# -gt 1 ]; do
		case "$1" in
		remove) rm -f "$S/p/$u/key-mgmt" "$S/p/$u/psk";;
		*) put "$u" "${1#*.}" "$2";;
		esac
		shift 2
	done
	[ -f "$S/modify-fails" ] && { echo "Error: killed after saving" >&2; exit 1; };;
"connection up") up "$3";;
"connection delete") rm -rf "$S/p/$3";;
*) echo "fake nmcli: unexpected: $*" >&2; exit 1;;
esac
exit 0
`

// homePassword contains the characters nmcli escapes, so a restore that is
// not byte-exact fails the tests.
const homePassword = `home:pass\word 1`

type fakeWiFi struct {
	t   *testing.T
	dir string
}

func newFakeWiFi(t *testing.T, apPassword string) (*NMCLINetworkManager, *fakeWiFi) {
	t.Helper()
	f := &fakeWiFi{t: t, dir: t.TempDir()}
	if err := os.Mkdir(filepath.Join(f.dir, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.write("ap", apPassword)
	f.write("active", "")
	path := filepath.Join(f.dir, "nmcli")
	if err := os.WriteFile(path, []byte(fakeNMCLIScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_NMCLI_STATE", f.dir)
	return &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}, f
}

func (f *fakeWiFi) write(name, value string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(value), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeWiFi) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(b)
}

// addProfile saves a profile; an empty keyMgmt makes it an open network.
func (f *fakeWiFi) addProfile(uuid, ssid, hidden, keyMgmt, psk string) {
	f.t.Helper()
	if err := os.Mkdir(filepath.Join(f.dir, "p", uuid), 0o700); err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join("p", uuid, "ssid"), ssid)
	f.write(filepath.Join("p", uuid, "hidden"), hidden)
	if keyMgmt != "" {
		f.write(filepath.Join("p", uuid, "key-mgmt"), keyMgmt)
		f.write(filepath.Join("p", uuid, "psk"), psk)
	}
}

func (f *fakeWiFi) field(uuid, name string) string { return f.read(filepath.Join("p", uuid, name)) }

func (f *fakeWiFi) exists(uuid string) bool {
	_, err := os.Stat(filepath.Join(f.dir, "p", uuid))
	return err == nil
}

func TestConnectToWiFiWrongPasswordKeepsSavedProfile(t *testing.T) {
	n, f := newFakeWiFi(t, homePassword)
	f.addProfile("home", "Home", "no", "wpa-psk", homePassword)
	f.write("active", "home")

	err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-password"})
	if err == nil {
		t.Fatal("connect with a wrong password succeeded")
	}
	if got := f.field("home", "psk"); got != homePassword {
		t.Fatalf("saved psk = %q, want %q", got, homePassword)
	}
	if got := f.read("active"); got != "home" {
		t.Fatalf("active profile = %q, want home reconnected", got)
	}
}

func TestConnectToWiFiProfilePathRestoresOverwrittenSettings(t *testing.T) {
	for _, tc := range []struct{ name, keyMgmt, psk string }{
		{name: "secured profile", keyMgmt: "sae", psk: homePassword},
		{name: "open profile", keyMgmt: "", psk: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newFakeWiFi(t, "ap-password")
			f.addProfile("home", "Home", "no", tc.keyMgmt, tc.psk)
			hidden := true
			sec := agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK

			err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{
				Ssid: "Home", Password: "wrong-password", Hidden: &hidden, Security: &sec,
			})
			if err == nil {
				t.Fatal("connect with a wrong password succeeded")
			}
			if got := f.field("home", "hidden"); got != "no" {
				t.Errorf("hidden = %q, want no", got)
			}
			if got := f.field("home", "key-mgmt"); got != tc.keyMgmt {
				t.Errorf("key-mgmt = %q, want %q", got, tc.keyMgmt)
			}
			if got := f.field("home", "psk"); got != tc.psk {
				t.Errorf("psk = %q, want %q", got, tc.psk)
			}
		})
	}
}

func TestConnectToWiFiRightPasswordUpdatesSavedProfile(t *testing.T) {
	n, f := newFakeWiFi(t, "new-password")
	f.addProfile("home", "Home", "no", "wpa-psk", "old-password")

	if err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "new-password"}); err != nil {
		t.Fatal(err)
	}
	if got := f.field("home", "psk"); got != "new-password" {
		t.Fatalf("saved psk = %q, want the new password", got)
	}
	if got := f.read("active"); got != "home" {
		t.Fatalf("active profile = %q, want home", got)
	}
}

func TestConnectToWiFiFailedNewNetworkIsRemoved(t *testing.T) {
	n, f := newFakeWiFi(t, "office-password")
	f.addProfile("office", "Office", "no", "wpa-psk", "office-password")
	f.write("active", "office")

	err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Guest", Password: "wrong-password"})
	if err == nil {
		t.Fatal("connect with a wrong password succeeded")
	}
	if f.exists("new") {
		t.Fatal("profile created by the failed attempt was kept")
	}
	if got := f.field("office", "psk"); got != "office-password" {
		t.Fatalf("office psk = %q, want it unchanged", got)
	}
	if got := f.read("active"); got != "office" {
		t.Fatalf("active profile = %q, want office reconnected", got)
	}
}

// nmcli cannot reuse an open profile for a secured network, so it adds a
// second profile for the same SSID; only that new one may be deleted.
func TestConnectToWiFiRemovesProfileAddedNextToSavedOne(t *testing.T) {
	n, f := newFakeWiFi(t, "cafe-password")
	f.addProfile("cafe", "Cafe", "no", "", "")

	if err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Cafe", Password: "wrong-password"}); err == nil {
		t.Fatal("connect with a wrong password succeeded")
	}
	if f.exists("new") {
		t.Fatal("profile created by the failed attempt was kept")
	}
	if !f.exists("cafe") || f.field("cafe", "key-mgmt") != "" {
		t.Fatal("saved open profile was not kept as it was")
	}
}

// nmcli can be killed after NetworkManager saved the modify, e.g. by a cancelled request.
func TestConnectToWiFiRollsBackProfileSavedByFailedModify(t *testing.T) {
	n, f := newFakeWiFi(t, homePassword)
	f.addProfile("home", "Home", "no", "wpa-psk", homePassword)
	f.write("modify-fails", "")
	hidden := true

	err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-password", Hidden: &hidden})
	if err == nil {
		t.Fatal("connect succeeded although preparing the profile failed")
	}
	if got := f.field("home", "psk"); got != homePassword {
		t.Fatalf("saved psk = %q, want %q", got, homePassword)
	}
	if got := f.field("home", "hidden"); got != "no" {
		t.Fatalf("hidden = %q, want no", got)
	}
}

// nmcli escapes ':' and '\' in terse output and never escapes a newline, so a
// SSID is only matched exactly when read raw, one profile at a time.
func TestConnectToWiFiMatchesUnusualSSIDsExactly(t *testing.T) {
	cafe := `Cafe: "A\B"`
	for _, tc := range []struct {
		name, password, wantPSK, wantActive string
		wantErr                             bool
	}{
		{name: "right password", password: "new-password", wantPSK: "new-password", wantActive: "cafe"},
		{name: "wrong password", password: "wrong-password", wantPSK: "old-password", wantActive: "odd", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newFakeWiFi(t, "new-password")
			f.addProfile("odd", "line1\nline2", "no", "wpa-psk", "new-password")
			f.addProfile("cafe", cafe, "no", "wpa-psk", "old-password")
			f.write("active", "odd")

			err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: cafe, Password: tc.password})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := f.field("cafe", "psk"); got != tc.wantPSK {
				t.Fatalf("cafe psk = %q, want %q", got, tc.wantPSK)
			}
			if got := f.read("active"); got != tc.wantActive {
				t.Fatalf("active profile = %q, want %q", got, tc.wantActive)
			}
		})
	}
}

// nmcli lists the active profile first, so Home and Alpha swap places once the
// attempt drops Home; SSIDs must still map to the right UUIDs.
func TestConnectToWiFiWrongPasswordWithSeveralSavedProfiles(t *testing.T) {
	n, f := newFakeWiFi(t, homePassword)
	f.addProfile("home", "Home", "no", "wpa-psk", homePassword)
	f.addProfile("alpha", "Alpha", "no", "wpa-psk", homePassword)
	f.write("active", "home")

	if err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Alpha", Password: "wrong-password"}); err == nil {
		t.Fatal("connect with a wrong password succeeded")
	}
	if !f.exists("alpha") {
		t.Fatal("saved profile for the retried network was deleted")
	}
	if got := f.field("alpha", "psk"); got != homePassword {
		t.Fatalf("alpha psk = %q, want %q", got, homePassword)
	}
	if got := f.read("active"); got != "home" {
		t.Fatalf("active profile = %q, want home reconnected", got)
	}
}

// A cancelled request kills nmcli, but NetworkManager may still be activating
// the attempt; the rollback restores the profile and re-activates it anyway.
func TestRollbackUndoesCancelledAttempt(t *testing.T) {
	for _, tc := range []struct{ name, activeBefore string }{
		{name: "home was active", activeBefore: "home"},
		{name: "no WiFi was active", activeBefore: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, f := newFakeWiFi(t, homePassword)
			f.addProfile("home", "Home", "no", "wpa-psk", homePassword)
			f.write("active", tc.activeBefore)
			before, err := takeWiFiSnapshot(context.Background(), n.nmcliPath, "Home")
			if err != nil {
				t.Fatal(err)
			}
			f.write(filepath.Join("p", "home", "psk"), "new-password")
			f.write("active", "home")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			n.rollbackFailedConnect(ctx, "Home", before)

			if got := f.field("home", "psk"); got != homePassword {
				t.Fatalf("saved psk = %q, want %q", got, homePassword)
			}
			if !strings.Contains(f.read("log"), "--wait 0 connection up home") {
				t.Fatal("home was not re-activated on its restored password")
			}
		})
	}
}

// The saved network is out of range, so nmcli fails before touching anything.
func TestRollbackLeavesUnchangedStateAlone(t *testing.T) {
	n, f := newFakeWiFi(t, homePassword)
	f.addProfile("home", "Home", "no", "wpa-psk", homePassword)
	f.addProfile("missing", "Missing", "no", "wpa-psk", "missing-password")
	f.write("active", "home")

	if err := n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Missing", Password: "any-password"}); err == nil {
		t.Fatal("connect to a missing network succeeded")
	}
	for _, line := range strings.Split(f.read("log"), "\n") {
		if strings.HasPrefix(line, "connection ") || strings.HasPrefix(line, "--wait") {
			t.Errorf("unexpected rollback step: %q", line)
		}
	}
}
