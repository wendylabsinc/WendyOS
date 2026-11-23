package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/proto"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestConnectFailure(t *testing.T) {
	cases := []struct {
		words       string
		hadPassword bool
		want        ConnectFailure
	}{
		// nmcli connection up, then nmcli device wifi connect with its reason number.
		{"Connection activation failed: Secrets were required, but not provided.", true, ConnectFailureWrongPassword},
		{"Connection activation failed: (7) Secrets were required, but not provided.", true, ConnectFailureWrongPassword},
		{"Connection activation failed: Secrets were required, but not provided.", false, ConnectFailurePasswordRequired},
		{"Connection activation failed: (7) Secrets were required, but not provided.", false, ConnectFailurePasswordRequired},
		{"CONNECTION ACTIVATION FAILED: SECRETS WERE REQUIRED, BUT NOT PROVIDED.", true, ConnectFailureWrongPassword},
		{"No network with SSID 'MyNet' found.", true, ConnectFailureNetworkNotFound},
		{"Connection activation failed: The Wi-Fi network could not be found", true, ConnectFailureNetworkNotFound},
		{"Connection activation failed: (53) The Wi-Fi network could not be found.", false, ConnectFailureNetworkNotFound},
		{"Failed to add 'MyNet' connection: 802-11-wireless-security.psk: property is invalid", true, ConnectFailureInvalidPassword},
		{"802-11-wireless-security.psk: property is invalid.", true, ConnectFailureInvalidPassword},
		{"Timeout expired (90 seconds)", true, ConnectFailureTimedOut},
		{"Connection activation failed: The connection attempt timed out", true, ConnectFailureTimedOut},
		{"Connection activation failed: (11) 802.1X supplicant took too long to authenticate.", true, ConnectFailureTimedOut},
		// This reason's own words mention a timeout.
		{"Connection activation failed: IP configuration could not be reserved (no available address, timeout, etc.)", true, ConnectFailureNoAddress},
		{"Connection activation failed: (5) IP configuration could not be reserved (no available address, timeout, etc.).", true, ConnectFailureNoAddress},
		{"No Wi-Fi device found.", false, ConnectFailureNoAdapter},
		{"No WiFi device found.", true, ConnectFailureNoAdapter},
		// A network's name is not nmcli's reason.
		{"No network with SSID 'no wifi device timeout' found.", true, ConnectFailureNetworkNotFound},
		{"Connection activation failed: (4) The device could not be readied for configuration.", true, ConnectFailureUnknown},
		{"802-11-wireless-security.psk: property is missing", true, ConnectFailureUnknown},
		{"", true, ConnectFailureUnknown},
	}
	for _, c := range cases {
		if got := connectFailure(c.words, c.hadPassword); got != c.want {
			t.Errorf("connectFailure(%q, %v) = %d; want %d", c.words, c.hadPassword, got, c.want)
		}
	}
}

func TestNMCLIWords(t *testing.T) {
	cases := map[string]string{
		"nmcli connection up My: Net: Error: Connection activation failed: Secrets were required, but not provided.: exit status 4":                                         "Connection activation failed: Secrets were required, but not provided.",
		"nmcli connect: Error: No network with SSID 'My: Net' found.: exit status 10":                                                                                       "No network with SSID 'My: Net' found.",
		"preparing WiFi profile: nmcli connection add My: Net: Error: Failed to add 'My: Net' connection: 802-11-wireless-security.psk: property is invalid: exit status 2": "Failed to add 'My: Net' connection: 802-11-wireless-security.psk: property is invalid",
		"preparing WiFi profile: nmcli connection modify My: Net: Error: 802-11-wireless-security.psk: property is invalid.: exit status 2":                                 "802-11-wireless-security.psk: property is invalid.",
		"nmcli connection up My: Net: Error: one\nHint: two: exit status 4":                                                                                                 "one\nHint: two",
		"nmcli connect: : exit status 1":  "",
		"nmcli connect: : signal: killed": "signal: killed",
		"preparing WiFi profile: finding the added profile: 2 saved profiles for SSID \"My: Net\"": "finding the added profile: 2 saved profiles for SSID \"My: Net\"",
		// Another network's name in the prefix is not this request's wrapper.
		"nmcli connection up Other: Error: failed: exit status 4": "nmcli connection up Other: Error: failed",
	}
	for text, want := range cases {
		if got := nmcliWords(text, "My: Net"); got != want {
			t.Errorf("nmcliWords(%q) = %q; want %q", text, got, want)
		}
	}
}

func TestConnectErrorWithoutNMCLIWords(t *testing.T) {
	for _, err := range []error{nil, errors.New("nmcli connect: : exit status 1")} {
		e := &ConnectError{SSID: "MyNet", Err: err}
		if got, want := e.Error(), `the device could not join "MyNet"`; got != want {
			t.Errorf("message for %v = %q; want %q", err, got, want)
		}
	}
}

// failingConnect runs ConnectToWiFi against a fake nmcli whose first call
// starting with fails prints out and exits with code. The network is saved
// nowhere, and scan is the cached scan.
func failingConnect(t *testing.T, logger *zap.Logger, req *agentpb.ConnectToWiFiRequest, scan, fails, out string, code int) error {
	t.Helper()
	path, _ := writeFakeNMCLI(t, append(severalProfiles(),
		reply{args: scanArgs, out: scan},
		reply{args: activeArgs},
		reply{args: fails, prefix: true, out: out, code: code},
		addOK,
		reply{args: "connection up ", prefix: true},
		reply{args: "connection delete ", prefix: true},
		reply{args: "device wifi connect ", prefix: true},
	)...)
	n := &NMCLINetworkManager{logger: logger, nmcliPath: path}
	return n.ConnectToWiFi(context.Background(), req)
}

func TestConnectToWiFiWordsFailures(t *testing.T) {
	inScan := scanRow("Lab", "WPA2")
	withPassword := &agentpb.ConnectToWiFiRequest{Ssid: "Lab", Password: "secret123"}
	cases := []struct {
		name             string
		req              *agentpb.ConnectToWiFiRequest
		scan, fails, out string
		code             int
		reason           ConnectFailure
		message          string
	}{
		{"wrong password for a network in the scan", withPassword, inScan,
			"connection up ", "Error: Connection activation failed: Secrets were required, but not provided.\n", 4,
			ConnectFailureWrongPassword, `"Lab" did not accept the password`},
		{"wrong password for a hidden network", &agentpb.ConnectToWiFiRequest{Ssid: "Lab", Password: "secret123", Hidden: proto.Bool(true)}, "",
			"connection up ", "Error: Connection activation failed: Secrets were required, but not provided.\n", 4,
			ConnectFailureWrongPassword, `"Lab" did not accept the password`},
		{"no password for a secured network", &agentpb.ConnectToWiFiRequest{Ssid: "Lab"}, inScan,
			"device wifi connect ", "Error: Connection activation failed: (7) Secrets were required, but not provided.\n", 4,
			ConnectFailurePasswordRequired, `"Lab" needs a password`},
		{"network out of range", withPassword, scanRow("Other", "WPA2"),
			"device wifi connect ", "Error: No network with SSID 'Lab' found.\n", 10,
			ConnectFailureNetworkNotFound, `no Wi-Fi network named "Lab" is in range of the device`},
		{"password too short", &agentpb.ConnectToWiFiRequest{Ssid: "Lab", Password: "short"}, inScan,
			"connection add ", "Error: Failed to add 'Lab' connection: 802-11-wireless-security.psk: property is invalid\n", 2,
			ConnectFailureInvalidPassword, "a Wi-Fi password is 8 to 63 characters"},
		{"network never answers", withPassword, inScan,
			"connection up ", "Error: Timeout expired (90 seconds)\n", 3,
			ConnectFailureTimedOut, `"Lab" did not answer in time; the device may be too far from it, or the password may be wrong`},
		{"network gives no address", withPassword, inScan,
			"connection up ", "Error: Connection activation failed: IP configuration could not be reserved (no available address, timeout, etc.)\n", 4,
			ConnectFailureNoAddress, `joined "Lab" but it gave the device no IP address`},
		{"no adapter", &agentpb.ConnectToWiFiRequest{Ssid: "Lab"}, "",
			"device wifi connect ", "Error: No Wi-Fi device found.\n", 10,
			ConnectFailureNoAdapter, "the device has no Wi-Fi adapter"},
		{"another failure through a profile", withPassword, inScan,
			"connection up ", "Error: Connection activation failed: The base network connection was interrupted\n", 4,
			ConnectFailureUnknown, "Connection activation failed: The base network connection was interrupted"},
		{"another failure through nmcli's connect", &agentpb.ConnectToWiFiRequest{Ssid: "Lab"}, inScan,
			"device wifi connect ", "Error: Connection activation failed: (4) The device could not be readied for configuration.\n", 4,
			ConnectFailureUnknown, "Connection activation failed: (4) The device could not be readied for configuration."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := failingConnect(t, zap.NewNop(), c.req, c.scan, c.fails, c.out, c.code)
			var failure *ConnectError
			if !errors.As(err, &failure) {
				t.Fatalf("error %v (%T); want a *ConnectError", err, err)
			}
			if failure.Reason != c.reason || failure.SSID != "Lab" {
				t.Errorf("reason %d for %q; want %d for Lab", failure.Reason, failure.SSID, c.reason)
			}
			if err.Error() != c.message {
				t.Errorf("message %q; want %q", err.Error(), c.message)
			}
			if raw := errors.Unwrap(err); raw == nil || !strings.Contains(raw.Error(), strings.TrimSpace(c.out)) {
				t.Errorf("unwraps to %v; want what nmcli said", raw)
			}
		})
	}
}

// Errors that say nothing about the network keep their own text and type.
func TestConnectToWiFiLeavesOtherErrorsAlone(t *testing.T) {
	req := &agentpb.ConnectToWiFiRequest{Ssid: "Lab", Password: "secret123"}
	notRunning := "Error: NetworkManager is not running.\n"
	cases := []struct {
		name        string
		replies     []reply
		unavailable bool
		text        string
	}{
		{"saved profiles can't be read",
			[]reply{{args: scanArgs}, {args: listUUIDs, out: "Error: Could not create NMClient object.\n", code: 1}},
			false, profileCheckPrefix},
		{"NetworkManager is not running",
			[]reply{{args: scanArgs}, {args: listUUIDs, out: notRunning, code: nmcliNotRunning}},
			true, profileCheckPrefix},
		{"NetworkManager stops during the connect",
			append(severalProfiles(), reply{args: scanArgs}, reply{args: "device wifi connect ", prefix: true, out: notRunning, code: nmcliNotRunning}),
			true, "nmcli connect: Error: NetworkManager is not running."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, _ := writeFakeNMCLI(t, c.replies...)
			err := connect(t, path, req)
			var failure *ConnectError
			if err == nil || errors.As(err, &failure) {
				t.Fatalf("error %v (%T); want the error as the helper returned it", err, err)
			}
			if !strings.HasPrefix(err.Error(), c.text) {
				t.Errorf("error %q; want it to start with %q", err, c.text)
			}
			if got := NetworkManagerUnavailable(err); got != c.unavailable {
				t.Errorf("NetworkManagerUnavailable = %v; want %v", got, c.unavailable)
			}
		})
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	killed := errors.New("nmcli connect: : signal: killed")
	if got := connectError(cancelled, req, killed); got != killed {
		t.Errorf("cancelled request: %v (%T); want the error unchanged", got, got)
	}
	missing := fmt.Errorf("connecting: %w", ErrNMCLINotFound)
	if got := connectError(context.Background(), req, missing); got != missing {
		t.Errorf("missing nmcli: %v (%T); want the error unchanged", got, got)
	}
}

// What nmcli said goes to the device log, not to the person connecting.
func TestConnectToWiFiLogsWhatNMCLISaid(t *testing.T) {
	const said = "Error: Connection activation failed: Secrets were required, but not provided."
	core, logs := observer.New(zap.WarnLevel)
	err := failingConnect(t, zap.New(core), &agentpb.ConnectToWiFiRequest{Ssid: "Lab", Password: "wrong-pass"},
		scanRow("Lab", "WPA2"), "connection up ", said+"\n", 4)
	if err == nil {
		t.Fatal("want the connect to fail")
	}
	for _, raw := range []string{"nmcli", "Error:", "Secrets", "exit status"} {
		if strings.Contains(err.Error(), raw) {
			t.Errorf("message %q carries nmcli's %q", err.Error(), raw)
		}
	}
	entries := logs.FilterMessage("WiFi connect failed").All()
	if len(entries) != 1 {
		t.Fatalf("%d log entries for the failure; want 1: %v", len(entries), logs.All())
	}
	fields := entries[0].ContextMap()
	if want := "nmcli connection up Lab: " + said + ": exit status 4"; fields["ssid"] != "Lab" || fields["error"] != want {
		t.Errorf("logged ssid %q, error %q; want Lab, %q", fields["ssid"], fields["error"], want)
	}
	for _, entry := range logs.All() {
		if strings.Contains(fmt.Sprint(entry.Message, entry.ContextMap()), "wrong-pass") {
			t.Errorf("log entry %q %v carries the password", entry.Message, entry.ContextMap())
		}
	}
}
