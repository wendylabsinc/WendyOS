package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type nearbyAgentFake struct {
	closed, scans      int
	connects           []string // SSIDs sent to the device
	passwords          []string
	connectedSSID      string
	refuseConnect      bool
	dropLink           bool
	identity           certs.WendyIdentity
	noIdentity         bool
	forgotten          []string
	disconnects        int
	statusAfterConnect string
}

func (f *nearbyAgentFake) WifiList() ([]*pb.WifiNetworkInfo, error) {
	f.scans++
	if f.dropLink {
		return nil, errors.New("reading response header: link dropped")
	}
	return []*pb.WifiNetworkInfo{{Ssid: "office"}, {Ssid: ""}, {Ssid: "lab"}}, nil
}
func (f *nearbyAgentFake) WifiStatus() (*pb.WifiStatusResponse, error) {
	if f.dropLink {
		return nil, errors.New("reading response header: link dropped")
	}
	if f.connectedSSID == "" {
		return &pb.WifiStatusResponse{}, nil
	}
	ssid := f.connectedSSID
	return &pb.WifiStatusResponse{Connected: true, Ssid: &ssid}, nil
}
func (f *nearbyAgentFake) WifiConnect(ssid, password string) error {
	f.connects = append(f.connects, ssid)
	f.passwords = append(f.passwords, password)
	if f.dropLink {
		return errors.New("sending command: link dropped")
	}
	if f.refuseConnect {
		return &ble.AgentError{Message: "agent error: no network named " + ssid}
	}
	if f.statusAfterConnect != "" {
		f.connectedSSID = f.statusAfterConnect
	} else {
		f.connectedSSID = ssid
	}
	return nil
}
func (f *nearbyAgentFake) WifiDisconnect() error { f.disconnects++; f.connectedSSID = ""; return nil }
func (f *nearbyAgentFake) WifiForget(ssid string) error {
	f.forgotten = append(f.forgotten, ssid)
	return nil
}
func (f *nearbyAgentFake) PeerIdentity() (certs.WendyIdentity, bool) {
	if f.noIdentity {
		return certs.WendyIdentity{}, false
	}
	if f.identity == (certs.WendyIdentity{}) {
		return certs.WendyIdentity{EntityType: certs.EntityAsset, EntityID: "device-uuid", TenantUUID: "tenant", Principal: "spiffe://wendy.sh/tenant/tenant/device/device-uuid"}, true
	}
	return f.identity, true
}
func (f *nearbyAgentFake) Close() { f.closed++ }

func nearbyWiFiTestHooks(t *testing.T) {
	t.Helper()
	certificates, connect := nearbyWiFiCertificatesFn, nearbyWiFiConnectFn
	t.Cleanup(func() { nearbyWiFiCertificatesFn, nearbyWiFiConnectFn = certificates, connect })
	nearbyWiFiCertificatesFn = func(context.Context, string) ([]config.CertificateInfo, error) {
		return []config.CertificateInfo{{OrganizationID: 7, PemCertificate: "org-7"}}, nil
	}
}

func TestNearbyWiFiRefusesToConnectWithoutCertificate(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	nearbyWiFiCertificatesFn = func(context.Context, string) ([]config.CertificateInfo, error) {
		return nil, errors.New("Sign in first")
	}
	attempts := 0
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) {
		attempts++
		return new(nearbyAgentFake), nil
	}
	s, err := b.NearbyWiFiOpen("board", "board", "account")
	if err == nil || s.Connected || attempts != 0 {
		t.Fatalf("opened the management door without a certificate: %+v %v attempts=%d", s, err, attempts)
	}
	if _, err := b.NearbyWiFiAction(s.Session, 2, "office", "hunter2"); err == nil {
		t.Fatal("sent credentials over an unauthenticated session")
	}
}

func TestNearbyWiFiUsesTheOrganizationTheDeviceProves(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	nearbyWiFiCertificatesFn = func(_ context.Context, account string) ([]config.CertificateInfo, error) {
		if account != "chosen" {
			t.Fatalf("account = %q", account)
		}
		return []config.CertificateInfo{{OrganizationID: 7, PemCertificate: "org-7"}, {OrganizationID: 9, PemCertificate: "org-9"}}, nil
	}
	var presented []string
	f := new(nearbyAgentFake)
	nearbyWiFiConnectFn = func(device *models.BluetoothDevice, cert config.CertificateInfo) (nearbyAgent, error) {
		presented = append(presented, cert.PemCertificate)
		if device.DisplayName != "kitchen-pi" || device.Address != "ble-address" || device.L2CAPPSM != ble.DefaultL2CAPPSM {
			t.Fatalf("device = %+v", device)
		}
		if cert.OrganizationID == 7 {
			return nil, fmt.Errorf("BLE mTLS handshake: %w", &certs.OrgMismatchError{Want: 7, Got: 9})
		}
		return f, nil
	}
	s, err := b.NearbyWiFiOpen("ble-address", "kitchen-pi", "chosen")
	if err != nil || !s.Connected || strings.Join(presented, ",") != "org-7,org-9" {
		t.Fatalf("did not fall back to the device's organization: %+v %v presented=%v", s, err, presented)
	}
	if s.DeviceID != "device-uuid" || s.Principal != "spiffe://wendy.sh/tenant/tenant/device/device-uuid" || s.Organization != "tenant" {
		t.Fatalf("identity not taken from the verified certificate: %+v", s)
	}
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) {
		return nil, errors.New("connecting to kitchen-pi: timeout")
	}
	if _, err := b.NearbyWiFiOpen("ble-address", "kitchen-pi", "chosen"); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("a transport failure must not be retried with another organization: %v", err)
	}
	if f.closed != 1 {
		t.Fatal("reopening did not close the previous session")
	}
}

func TestNearbyWiFiRejectsDeviceWithoutVerifiedIdentity(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	f := &nearbyAgentFake{noIdentity: true}
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) { return f, nil }
	s, err := b.NearbyWiFiOpen("board", "board", "")
	if err == nil || s.Connected || f.closed != 1 {
		t.Fatalf("accepted a peer without a device identity: %+v %v", s, err)
	}
	f = &nearbyAgentFake{identity: certs.WendyIdentity{EntityType: "user", EntityID: "someone", TenantUUID: "tenant"}}
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) { return f, nil }
	if s, err := b.NearbyWiFiOpen("board", "board", ""); err == nil || s.Connected || f.closed != 1 {
		t.Fatalf("accepted a non-device identity: %+v %v", s, err)
	}
}

func TestNearbyWiFiConnectConfirmsRequestedNetworkAndKeepsNoPassword(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	f := &nearbyAgentFake{statusAfterConnect: "other-network"}
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) { return f, nil }
	s, err := b.NearbyWiFiOpen("board", "board", "")
	if err != nil || !s.Connected || s.WiFiConnected {
		t.Fatalf("open: %+v %v", s, err)
	}
	for _, bad := range [][2]string{{"", "x"}, {strings.Repeat("s", 33), "x"}, {"office", strings.Repeat("p", 65)}, {"off\x00ice", "x"}} {
		if _, err := b.NearbyWiFiAction(s.Session, 2, bad[0], bad[1]); err == nil || len(f.connects) != 0 {
			t.Fatalf("invalid credentials %q reached the device: %v", bad, err)
		}
	}
	s, err = b.NearbyWiFiAction(s.Session, 2, "office", "hunter2")
	if err == nil || s.WiFiConnected && s.SSID == "office" || !s.Connected {
		t.Fatalf("a different connected network was reported as success: %+v %v", s, err)
	}
	f.statusAfterConnect = ""
	s, err = b.NearbyWiFiAction(s.Session, 2, "office", "hunter2")
	if err != nil || !s.WiFiConnected || s.SSID != "office" || !s.Connected {
		t.Fatalf("confirmed connection: %+v %v", s, err)
	}
	if len(f.connects) != 2 || f.passwords[0] != "hunter2" || f.passwords[1] != "hunter2" {
		t.Fatalf("credentials sent = %v", f.connects)
	}
	if text := fmt.Sprintf("%+v", s); strings.Contains(text, "hunter2") {
		t.Fatalf("state carries the password: %s", text)
	}
	if text := fmt.Sprintf("%+v", b.nearbyWiFi.state); strings.Contains(text, "hunter2") {
		t.Fatalf("session retains the password: %s", text)
	}
}

func TestNearbyWiFiDeviceRefusalKeepsSessionAndLinkLossEndsIt(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	f := &nearbyAgentFake{refuseConnect: true}
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) { return f, nil }
	s, _ := b.NearbyWiFiOpen("board", "board", "")
	s, err := b.NearbyWiFiAction(s.Session, 2, "office", "hunter2")
	if err == nil || !s.Connected || f.closed != 0 || strings.Contains(err.Error(), "no network named") {
		t.Fatalf("device refusal: %+v %v", s, err)
	}
	s, err = b.NearbyWiFiAction(s.Session, 1, "", "")
	if err != nil || strings.Join(s.Networks, ",") != "office,lab" {
		t.Fatalf("scan after refusal: %+v %v", s, err)
	}
	f.dropLink = true
	s, err = b.NearbyWiFiAction(s.Session, 1, "", "")
	if err == nil || s.Connected || f.closed != 1 {
		t.Fatalf("link loss kept the session alive: %+v %v", s, err)
	}
	if _, err := b.NearbyWiFiAction(s.Session, 2, "office", "hunter2"); err == nil || len(f.connects) != 1 {
		t.Fatal("sent credentials over a lost link")
	}
}

func TestNearbyWiFiDisconnectAndForgetRefreshStatus(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	f := &nearbyAgentFake{connectedSSID: "office"}
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) { return f, nil }
	s, err := b.NearbyWiFiOpen("board", "board", "")
	if err != nil || !s.WiFiConnected || s.SSID != "office" {
		t.Fatalf("open: %+v %v", s, err)
	}
	if s, err = b.NearbyWiFiAction(s.Session, 3, "", ""); err != nil || s.WiFiConnected || f.disconnects != 1 {
		t.Fatalf("disconnect: %+v %v", s, err)
	}
	if _, err = b.NearbyWiFiAction(s.Session, 4, "", ""); err == nil || len(f.forgotten) != 0 {
		t.Fatal("forgot an unnamed network")
	}
	if s, err = b.NearbyWiFiAction(s.Session, 4, "office", ""); err != nil || strings.Join(f.forgotten, ",") != "office" {
		t.Fatalf("forget: %+v %v", s, err)
	}
}

func TestNearbyWiFiStaleSessionCannotActOrCloseReplacement(t *testing.T) {
	nearbyWiFiTestHooks(t)
	b := new(Backend)
	var current *nearbyAgentFake
	nearbyWiFiConnectFn = func(*models.BluetoothDevice, config.CertificateInfo) (nearbyAgent, error) {
		current = new(nearbyAgentFake)
		return current, nil
	}
	old, _ := b.NearbyWiFiOpen("board", "board", "")
	next, _ := b.NearbyWiFiOpen("board", "board", "")
	b.NearbyWiFiClose(old.Session)
	if current.closed != 0 {
		t.Fatal("stale cleanup closed replacement session")
	}
	if _, err := b.NearbyWiFiAction(old.Session, 0, "", ""); err == nil {
		t.Fatal("stale action accepted")
	}
	if _, err := b.NearbyWiFiAction(0, 0, "", ""); err == nil {
		t.Fatal("zero session accepted")
	}
	b.NearbyWiFiClose(next.Session)
	if current.closed != 1 {
		t.Fatal("active session did not close")
	}
	if _, err := b.NearbyWiFiAction(next.Session, 0, "", ""); err == nil {
		t.Fatal("closed session accepted an action")
	}
}

func TestNearbyEnrolledAdvertisementNeedsManagementDoorOnly(t *testing.T) {
	setup, agent := "7565E9EB-4C20-4B67-9272-D708B397B632", "7565E9EB-4C20-4B67-9272-D708B397B631"
	cases := map[bool][][]string{
		true:  {{agent}, {"180F", agent}},
		false: {{setup}, {setup, agent}, {agent, setup}, nil, {"180F"}},
	}
	for want, lists := range cases {
		for _, uuids := range lists {
			if got := nearbyEnrolledAdvertisement(scan.BLEDeviceInfo{ServiceUUIDs: uuids}); got != want {
				t.Fatalf("enrolled(%v) = %v, want %v", uuids, got, want)
			}
		}
	}
}
