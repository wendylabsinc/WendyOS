package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/proto"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestParseProfileSnapshot(t *testing.T) {
	secured := func(hidden, keyMgmt, authAlg, psk string) string {
		return hiddenField + ":" + hidden + "\n" + keyMgmtField + ":" + keyMgmt + "\n" +
			authAlgField + ":" + authAlg + "\n" + pskField + ":" + psk + "\n"
	}
	cases := []struct {
		name string
		out  string
		want profileSnapshot
	}{
		{"wpa2", secured("no", "wpa-psk", "", "home-pass"), profileSnapshot{"no", "wpa-psk", "", "home-pass"}},
		{"hidden wpa3 with forced open auth", secured("yes", "sae", "open", "home-pass"),
			profileSnapshot{"yes", "sae", "open", "home-pass"}},
		{"open network", hiddenField + ":no\n", profileSnapshot{hidden: "no"}},
		{"wep without a passphrase", secured("no", "none", "", ""), profileSnapshot{"no", "none", "", ""}},
		{"colon, backslash and spaces kept raw", secured("no", "wpa-psk", "", ` a:b\c `),
			profileSnapshot{"no", "wpa-psk", "", ` a:b\c `}},
		{"passphrase that spans lines", secured("no", "sae", "", "one\n"+keyMgmtField+":wpa-psk"),
			profileSnapshot{"no", "sae", "", "one\n" + keyMgmtField + ":wpa-psk"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseProfileSnapshot(c.out)
			if err != nil || got != c.want {
				t.Fatalf("parseProfileSnapshot(%q) = %+v, %v; want %+v", c.out, got, err, c.want)
			}
		})
	}
	for _, out := range []string{
		"",
		"Error: something else\n",
		hiddenField + ":no\n" + pskField + ":home-pass\n",
		hiddenField + ":no\n" + keyMgmtField + ":wpa-psk\n" + authAlgField + ":\n",
	} {
		_, err := parseProfileSnapshot(out)
		if err == nil {
			t.Errorf("parseProfileSnapshot(%q) must be an error", out)
		} else if strings.Contains(err.Error(), "home-pass") {
			t.Errorf("error %q quotes the passphrase", err)
		}
	}
}

// connectAndCancel starts a connect and cancels its request once the fake nmcli
// has made the file hung, which it does just before it blocks.
func connectAndCancel(t *testing.T, n *NMCLINetworkManager, hung string, req *agentpb.ConnectToWiFiRequest) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.ConnectToWiFi(ctx, req) }()
	deadline := time.After(20 * time.Second)
	for {
		if _, err := os.Stat(hung); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("connect returned %v before nmcli blocked", err)
		case <-deadline:
			t.Fatal("nmcli never blocked")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the cancelled connect")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("connect did not return after its request was cancelled")
	}
}

// hang blocks the fake nmcli until it is killed. exec leaves no child holding
// the output pipe open once that happens.
const hang = `touch "$d/hung"; exec sleep 60`

// A client that cancels still gets the profile its attempt created deleted.
func TestConnectCancelledDeletesAddedProfile(t *testing.T) {
	t.Run("profile brought up by the agent", func(t *testing.T) {
		path, log := writeNMCLIScript(t, replyArms(reply{args: listUUIDs}, addOK)+
			`"connection up "*) `+hang+` ;;
"connection delete "*) exit 0 ;;
`)
		n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
		connectAndCancel(t, n, filepath.Join(filepath.Dir(path), "hung"),
			&agentpb.ConnectToWiFiRequest{Ssid: "Hidden", Password: "secret123", Hidden: proto.Bool(true)})
		if dels := callsWithPrefix(readCalls(t, log), "connection delete "); len(dels) != 1 || dels[0] != "connection delete "+newUUID {
			t.Errorf("deleted %q; want the profile the cancelled attempt added", dels)
		}
	})
	t.Run("profile created by nmcli", func(t *testing.T) {
		path, log := writeNMCLIScript(t, `"`+listUUIDs+`") [ -f "$d/hung" ] && echo "uuid-x:802-11-wireless"; exit 0 ;;
"`+batchArgs("uuid-x")+`") printf '%s' '`+batchRecord("uuid-x", "wpa-psk", "", "Lab")+`'; exit 0 ;;
"`+scanArgs+`") exit 0 ;;
"device wifi connect "*) `+hang+` ;;
"connection delete "*) exit 0 ;;
`)
		n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
		connectAndCancel(t, n, filepath.Join(filepath.Dir(path), "hung"),
			&agentpb.ConnectToWiFiRequest{Ssid: "Lab", Password: "secret123"})
		if dels := callsWithPrefix(readCalls(t, log), "connection delete "); len(dels) != 1 || dels[0] != "connection delete uuid-x" {
			t.Errorf("deleted %q; want the profile nmcli created for the cancelled attempt", dels)
		}
	})
}

// snapshotArgs is the argument string readProfileSnapshot passes, less the UUID.
const snapshotArgs = "-s --escape no -t -f " + snapshotFields + " connection show "

// storeArms make the fake nmcli keep the fields a connect attempt may change in
// files named "<uuid>.<field>", so the attempt's writes and reads work on real
// state. `connection up` succeeds only when the saved passphrase is the one in
// the file ap-psk, the way an access point decides. Other files change one
// call: "unreadable" fails the read with secrets, "hang-save" and "hang-up"
// block a save (once applied) or an activation, and "reject" holds a
// passphrase that nmcli refuses to save, quoting it.
var storeArms = `"` + snapshotArgs + `"*)
	[ -f "$d/unreadable" ] && { echo "Error: timeout."; exit 1; }
	for u; do :; done
	for f in ` + strings.ReplaceAll(snapshotFields, ",", " ") + `; do
		[ -f "$d/$u.$f" ] && printf '%s:%s\n' "$f" "$(cat "$d/$u.$f")"
	done
	exit 0 ;;
"connection modify "*)
	if [ -f "$d/reject" ] && printf '%s' "$*" | grep -qF "$(cat "$d/reject")"; then
		echo "Error: '$(cat "$d/reject")' is not a valid PSK."
		exit 1
	fi
	u=$3; shift 3
	while [ $# -gt 0 ]; do
		if [ "$1" = remove ]; then rm -f "$d/$u.$2".*; else printf '%s' "$2" > "$d/$u.$1"; fi
		shift 2
	done
	[ -f "$d/hang-save" ] && { rm "$d/hang-save"; ` + hang + `; }
	exit 0 ;;
"connection up "*)
	[ -f "$d/hang-up" ] && { rm "$d/hang-up"; ` + hang + `; }
	[ "$(cat "$d/$3.` + pskField + `" 2>/dev/null)" = "$(cat "$d/ap-psk")" ] && exit 0
	echo "Error: Connection activation failed: Secrets were required, but not provided."
	exit 4 ;;
`

const homeUUID = "uuid-a"

// homeNet is a fake nmcli with one saved profile, homeUUID, for the WPA2
// network "Home" that wlan0 sees.
type homeNet struct {
	t    *testing.T
	n    *NMCLINetworkManager
	dir  string
	log  string
	logs *observer.ObservedLogs
}

// newHomeNet saves the profile as saved, active on wlan0 or not, for a network
// that takes the passphrase apPSK.
func newHomeNet(t *testing.T, saved profileSnapshot, active bool, apPSK string) *homeNet {
	t.Helper()
	activeOut := ""
	if active {
		activeOut = homeUUID + ":wlan0\n"
	}
	path, log := writeNMCLIScript(t, replyArms(append(
		savedFixture(savedRecord{homeUUID, profileOut(saved.keyMgmt, saved.authAlg, "Home")}),
		reply{args: scanArgs, out: scanRow("Home", "WPA2")},
		reply{args: activeArgs, out: activeOut},
	)...)+storeArms)
	core, logs := observer.New(zap.DebugLevel)
	h := &homeNet{t: t, dir: filepath.Dir(path), log: log, logs: logs,
		n: &NMCLINetworkManager{logger: zap.New(core), nmcliPath: path}}
	h.write("ap-psk", apPSK)
	h.write(homeUUID+"."+hiddenField, saved.hidden)
	if saved.keyMgmt != "" {
		h.write(homeUUID+"."+keyMgmtField, saved.keyMgmt)
		h.write(homeUUID+"."+authAlgField, saved.authAlg)
		h.write(homeUUID+"."+pskField, saved.psk)
	}
	return h
}

func (h *homeNet) write(name, value string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(value), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// saved reads the profile back from the fake's files.
func (h *homeNet) saved() profileSnapshot {
	h.t.Helper()
	read := func(field string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(h.dir, homeUUID+"."+field))
		return string(b), err == nil
	}
	var s profileSnapshot
	s.hidden, _ = read(hiddenField)
	if keyMgmt, ok := read(keyMgmtField); ok {
		s.keyMgmt = keyMgmt
		s.authAlg, _ = read(authAlgField)
		s.psk, _ = read(pskField)
	}
	return s
}

// calls returns the nmcli calls that start with prefix, once any activation
// started for a restored profile has ended.
func (h *homeNet) calls(prefix string) []string {
	h.t.Helper()
	h.n.reactivating.Wait()
	return callsWithPrefix(readCalls(h.t, h.log), prefix)
}

// checkNoPassphraseLogged fails if a log entry holds one of the passphrases.
func (h *homeNet) checkNoPassphraseLogged(passphrases ...string) {
	h.t.Helper()
	h.n.reactivating.Wait()
	for _, e := range h.logs.All() {
		line := e.Message + " " + fmt.Sprint(e.ContextMap())
		for _, p := range passphrases {
			if strings.Contains(line, p) {
				h.t.Errorf("log entry %q holds the passphrase %q", line, p)
			}
		}
	}
}

// A failed attempt leaves the saved profile it used as it was, and the device
// on the network it was on.
func TestConnectFailureRestoresSavedProfile(t *testing.T) {
	up, upOnWlan0 := "connection up "+homeUUID, "connection up "+homeUUID+" ifname wlan0"
	cases := []struct {
		name   string
		saved  profileSnapshot
		active bool
		req    *agentpb.ConnectToWiFiRequest
		ups    []string
	}{
		{"network in the scan, active profile",
			profileSnapshot{"no", "wpa-psk", "open", "home-pass"}, true,
			&agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass"},
			[]string{upOnWlan0, upOnWlan0}},
		{"security hint that changes key-mgmt and sets hidden",
			profileSnapshot{"no", "wpa-psk", "", "home-pass"}, true,
			&agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass", Hidden: proto.Bool(true),
				Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE.Enum()},
			[]string{up, upOnWlan0}},
		{"security hint on the profile of an open network",
			profileSnapshot{hidden: "no"}, true,
			&agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass",
				Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK.Enum()},
			[]string{up, upOnWlan0}},
		{"profile that was not active",
			profileSnapshot{"no", "wpa-psk", "", "home-pass"}, false,
			&agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass"},
			[]string{upOnWlan0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHomeNet(t, c.saved, c.active, c.saved.psk)
			if err := h.n.ConnectToWiFi(context.Background(), c.req); err == nil {
				t.Fatal("want the activation error")
			}
			if got := h.saved(); got != c.saved {
				t.Errorf("saved profile is %+v after the failed attempt; want %+v", got, c.saved)
			}
			// The second one brings the restored profile back up; the fake lets
			// it through only with the passphrase that was saved.
			checkCalls(t, h.calls("connection up "), c.ups)
			if got := h.logs.FilterMessageSnippet("Returned to the WiFi network").Len(); got != len(c.ups)-1 {
				t.Errorf("%d activations of the restored profile succeeded; want %d", got, len(c.ups)-1)
			}
			h.checkNoPassphraseLogged("home-pass", "wrong-pass")
		})
	}
}

// Cancelling the request undoes the attempt like a failure does.
func TestConnectCancelledRestoresSavedProfile(t *testing.T) {
	for name, marker := range map[string]string{"while activating": "hang-up", "while saving": "hang-save"} {
		t.Run(name, func(t *testing.T) {
			saved := profileSnapshot{"no", "wpa-psk", "", "home-pass"}
			h := newHomeNet(t, saved, true, "home-pass")
			h.write(marker, "")
			connectAndCancel(t, h.n, filepath.Join(h.dir, "hung"),
				&agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass"})
			ups := h.calls("connection up ")
			if got := h.saved(); got != saved {
				t.Errorf("saved profile is %+v after the cancelled attempt; want %+v", got, saved)
			}
			if len(ups) == 0 || ups[len(ups)-1] != "connection up "+homeUUID+" ifname wlan0" {
				t.Errorf("activations %q; want the restored profile brought back up on wlan0", ups)
			}
		})
	}
}

// Without a snapshot the profile can't be put back; the log says so.
func TestConnectFailureWithoutSnapshotLogs(t *testing.T) {
	h := newHomeNet(t, profileSnapshot{"no", "wpa-psk", "", "home-pass"}, true, "home-pass")
	h.write("unreadable", "")
	if err := h.n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass"}); err == nil {
		t.Fatal("want the activation error")
	}
	if got := h.saved().psk; got != "wrong-pass" {
		t.Errorf("saved passphrase is %q; want it left as the attempt wrote it", got)
	}
	if mods, ups := h.calls("connection modify "), h.calls("connection up "); len(mods) != 1 || len(ups) != 1 {
		t.Errorf("modified %d times and activated %d times; want only the attempt's own calls", len(mods), len(ups))
	}
	if got := h.logs.FilterMessageSnippet("may have been left changed").Len(); got != 1 {
		t.Errorf("%d log entries say the profile may have been left changed; want 1", got)
	}
	h.checkNoPassphraseLogged("home-pass", "wrong-pass")
}

// nmcli can quote a passphrase it refuses; that reply must not reach the log.
func TestConnectFailedRestoreLogsWithoutPassphrase(t *testing.T) {
	h := newHomeNet(t, profileSnapshot{"no", "wpa-psk", "", "home-pass"}, true, "home-pass")
	h.write("reject", "home-pass")
	if err := h.n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "wrong-pass"}); err == nil {
		t.Fatal("want the activation error")
	}
	if mods, ups := h.calls("connection modify "), h.calls("connection up "); len(mods) != 2 || len(ups) != 1 {
		t.Errorf("modified %d times and activated %d times; want a restore attempt and no activation after it failed", len(mods), len(ups))
	}
	if got := h.logs.FilterMessageSnippet("may have been left changed").Len(); got != 1 {
		t.Errorf("%d log entries say the profile may have been left changed; want 1", got)
	}
	h.checkNoPassphraseLogged("home-pass", "wrong-pass")
}

// The snapshot is only for failures: a connect that works keeps what it saved.
func TestConnectSuccessKeepsNewPassword(t *testing.T) {
	h := newHomeNet(t, profileSnapshot{"no", "wpa-psk", "", "old-pass"}, false, "new-pass")
	if err := h.n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "new-pass"}); err != nil {
		t.Fatal(err)
	}
	if got := h.saved().psk; got != "new-pass" {
		t.Errorf("saved passphrase is %q; want the new one", got)
	}
	if reads := h.calls(snapshotArgs); len(reads) != 1 {
		t.Errorf("profile read with its secrets %d times; want once, before the attempt", len(reads))
	}
	if mods, ups := h.calls("connection modify "), h.calls("connection up "); len(mods) != 1 || len(ups) != 1 {
		t.Errorf("modified %d times and activated %d times; want only the attempt's own calls", len(mods), len(ups))
	}
	h.checkNoPassphraseLogged("old-pass", "new-pass")
}

// An attempt with the passphrase already saved changes nothing, so its failure
// has nothing to put back and takes no network from the device.
func TestConnectSamePasswordNeedsNoRestore(t *testing.T) {
	h := newHomeNet(t, profileSnapshot{"no", "wpa-psk", "", "home-pass"}, true, "other-pass")
	if err := h.n.ConnectToWiFi(context.Background(), &agentpb.ConnectToWiFiRequest{Ssid: "Home", Password: "home-pass"}); err == nil {
		t.Fatal("want the activation error")
	}
	if reads := h.calls(snapshotArgs); len(reads) != 2 {
		t.Errorf("profile read with its secrets %d times; want before and after the attempt", len(reads))
	}
	if mods, ups := h.calls("connection modify "), h.calls("connection up "); len(mods) != 1 || len(ups) != 1 {
		t.Errorf("modified %d times and activated %d times; want only the attempt's own calls", len(mods), len(ups))
	}
}
