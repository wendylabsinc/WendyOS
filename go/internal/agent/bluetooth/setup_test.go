package bluetooth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type setupProvisionerFake struct {
	enrolled    bool
	unavailable bool
	enrolls     int
}

func (p *setupProvisionerFake) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	if p.unavailable {
		return nil, errors.New("unavailable")
	}
	if p.enrolled {
		return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{Provisioned: &agentpbv2.ProvisionedResponse{PrincipalUri: "device"}}}, nil
	}
	yes := true
	return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpbv2.NotProvisionedResponse{AcmeEnrollmentSupported: &yes}}}, nil
}
func (p *setupProvisionerFake) StartACMEProvisioning(context.Context, *agentpbv2.StartACMEProvisioningRequest) (*agentpbv2.StartACMEProvisioningResponse, error) {
	p.enrolls++
	p.enrolled = true
	return &agentpbv2.StartACMEProvisioningResponse{PrincipalUri: "device"}, nil
}

func TestSetupFailsClosedAndNeverDispatchesManagement(t *testing.T) {
	p := &setupProvisionerFake{}
	d := NewSetupDispatcher(p, nil)
	ctx := context.Background()
	for _, cmd := range []*agentpb.BluetoothCommand{
		{Command: &agentpb.BluetoothCommand_AppsList{AppsList: &agentpb.AppsListCommand{}}},
		{Command: &agentpb.BluetoothCommand_AppsRemove{AppsRemove: &agentpb.AppsRemoveCommand{AppName: "sensitive"}}},
		{Command: &agentpb.BluetoothCommand_WifiDisconnect{WifiDisconnect: &agentpb.WifiDisconnectCommand{}}},
		{Command: &agentpb.BluetoothCommand_SetupHello{SetupHello: &agentpb.SetupHello{}}},
	} {
		if d.Dispatch(ctx, cmd).GetError() == nil {
			t.Fatal("setup accepted non-setup operation")
		}
	}
	enroll := &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_SetupEnroll{SetupEnroll: &agentpb.SetupEnrollCommand{DeviceId: "device"}}}
	if !d.Available(ctx) {
		t.Fatal("unenrolled agent unavailable")
	}
	if d.Dispatch(ctx, enroll).GetSetupEnroll().GetPrincipalUri() != "device" {
		t.Fatal("enrollment failed")
	}
	if d.Available(ctx) {
		t.Fatal("enrolled agent advertised setup")
	}
	if d.Dispatch(ctx, enroll).GetError() == nil || p.enrolls != 1 {
		t.Fatal("re-enrolled an enrolled agent")
	}
	wifi := &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiConnect{WifiConnect: &agentpb.WifiConnectCommand{Ssid: "other"}}}
	if d.Dispatch(ctx, wifi).GetError() == nil {
		t.Fatal("enrolled Wi-Fi changed")
	}
	p.unavailable = true
	if d.Available(ctx) || d.Dispatch(ctx, enroll).GetError() == nil {
		t.Fatal("failed-open enrollment state")
	}
}

func TestSetupEncryptionDirectionsTamperAndReplay(t *testing.T) {
	client, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server, public, err := newSetupCipher(client.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	peer, _ := ecdh.X25519().NewPublicKey(public)
	shared, _ := client.ECDH(peer)
	keys, _ := hkdf.Key(sha256.New, shared, append(client.PublicKey().Bytes(), public...), "wendy-ble-setup-v1", 64)
	block, _ := aes.NewCipher(keys[:32])
	write, _ := cipher.NewGCM(block)
	block, _ = aes.NewCipher(keys[32:])
	read, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], 1)
	packet := write.Seal(append([]byte{}, nonce...), nonce, []byte("wifi password"), nil)
	tampered := append([]byte{}, packet...)
	tampered[len(tampered)-1] ^= 1
	if _, err := server.open(tampered); err == nil {
		t.Fatal("accepted tampered secret")
	}
	decoded, err := server.open(packet)
	if err != nil || string(decoded) != "wifi password" {
		t.Fatal("request did not decrypt", err)
	}
	if _, err := server.open(packet); err == nil {
		t.Fatal("accepted replay")
	}
	reply := server.seal([]byte("connected"))
	decoded, err = read.Open(nil, reply[:12], reply[12:], nil)
	if err != nil || string(decoded) != "connected" {
		t.Fatal("response did not decrypt", err)
	}
	if _, _, err := newSetupCipher(make([]byte, 32)); err == nil {
		t.Fatal("accepted low-order public key")
	}
}

func TestSetupCipherWireVector(t *testing.T) {
	data, err := os.ReadFile("testdata/setup-wire-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	decode := func(field string) []byte {
		b, err := hex.DecodeString(fixture[field])
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	private := make([]byte, 32)
	for i := range private {
		private[i] = byte(i + 33)
	}
	key, err := ecdh.X25519().NewPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := setupCipherWithKey(key, decode("client"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := c.open(decode("request"))
	if err != nil || string(plain) != "wifi password" {
		t.Fatal("incompatible client wire format", err)
	}
	if hex.EncodeToString(c.seal([]byte("connected"))) != fixture["response"] {
		t.Fatal("incompatible server wire format")
	}
}

type setupNetworkFake struct{ request *agentpb.ConnectToWiFiRequest }

func (n *setupNetworkFake) ListWiFiNetworks(context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	return []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{{Ssid: "lab"}}, nil
}
func (n *setupNetworkFake) ConnectToWiFi(_ context.Context, r *agentpb.ConnectToWiFiRequest) error {
	n.request = r
	return nil
}
func (n *setupNetworkFake) GetWiFiStatus(context.Context) (bool, string, error) {
	return n.request != nil, "lab", nil
}
func (n *setupNetworkFake) DisconnectWiFi(context.Context) error {
	return errors.New("must not be called")
}

func TestSetupWiFiUsesDeviceNetworkManager(t *testing.T) {
	p := &setupProvisionerFake{}
	n := &setupNetworkFake{}
	d := NewSetupDispatcher(p, n)
	ctx := context.Background()
	list := d.Dispatch(ctx, &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiList{WifiList: &agentpb.WifiListCommand{}}})
	if len(list.GetWifiList().GetNetworks()) != 1 || list.GetWifiList().GetNetworks()[0].GetSsid() != "lab" {
		t.Fatal("missing scanned SSID")
	}
	connect := &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiConnect{WifiConnect: &agentpb.WifiConnectCommand{Ssid: "lab", Password: "fixture-password"}}}
	if !d.Dispatch(ctx, connect).GetWifiConnect().GetSuccess() || n.request.GetPassword() != "fixture-password" {
		t.Fatal("Wi-Fi request not forwarded")
	}
	status := d.Dispatch(ctx, &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_WifiStatus{WifiStatus: &agentpb.WifiStatusCommand{}}})
	if !status.GetWifiStatus().GetConnected() || status.GetWifiStatus().GetSsid() != "lab" {
		t.Fatal("Wi-Fi status not returned")
	}
	p.enrolled = true
	n.request = nil
	if d.Dispatch(ctx, connect).GetError() == nil || n.request != nil {
		t.Fatal("setup changed enrolled Wi-Fi")
	}
}
