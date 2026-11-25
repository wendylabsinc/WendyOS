package backend

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/ble"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/nearbysetup"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// NearbyWiFiState describes the Bluetooth management session with an
// enrolled device. Unlike the setup door, this door is authenticated both
// ways: the device proved the identity below with its enrolled certificate,
// and accepted one of the account's operator certificates.
type NearbyWiFiState struct {
	Session                  uint64
	Connected, WiFiConnected bool
	SSID                     string
	// DeviceID matches the device list: the device UUID of a tenant principal,
	// or the legacy asset id. Principal is the full verified identity key.
	DeviceID, Principal, Organization string
	Networks                          []string
}

// nearbyAgent is the slice of ble.AgentClient this session uses.
type nearbyAgent interface {
	WifiList() ([]*pb.WifiNetworkInfo, error)
	WifiStatus() (*pb.WifiStatusResponse, error)
	WifiConnect(ssid, password string) error
	WifiDisconnect() error
	WifiForget(ssid string) error
	PeerIdentity() (certs.WendyIdentity, bool)
	Close()
}

var nearbyWiFiCertificatesFn = commands.DesktopBLEAgentCertificates
var nearbyWiFiConnectFn = func(device *models.BluetoothDevice, cert config.CertificateInfo) (nearbyAgent, error) {
	tlsConfig, err := commands.DesktopBLEAgentTLSConfig(device, cert)
	if err != nil {
		return nil, err
	}
	client, err := ble.ConnectAgent(device, tlsConfig)
	if err != nil {
		return nil, err
	}
	return client, nil
}

type nearbyWiFi struct {
	mu     sync.Mutex
	next   uint64
	state  NearbyWiFiState
	client nearbyAgent
}

// nearbyEnrolledAdvertisement reports a device that advertises the
// authenticated management door and not the setup door: the agent stops the
// latter once a device is enrolled (agent/bluetooth/setup_linux.go). A device
// seen with both is still in transition and keeps the setup path.
func nearbyEnrolledAdvertisement(d scan.BLEDeviceInfo) bool {
	setup, agent := false, false
	for _, uuid := range d.ServiceUUIDs {
		switch strings.ToLower(uuid) {
		case nearbysetup.ServiceUUID:
			setup = true
		case ble.AgentServiceUUID:
			agent = true
		}
	}
	return agent && !setup
}

func (n *nearbyWiFi) snapshot() NearbyWiFiState {
	s := n.state
	s.Networks = append([]string(nil), s.Networks...)
	return s
}

func (n *nearbyWiFi) closeLocked() {
	if n.client != nil {
		n.client.Close()
		n.client = nil
	}
	n.state.Connected = false
}

// NearbyWiFiOpen connects to an enrolled device's management door with the
// account's operator certificate. Without a certificate nothing is sent to the
// device. account names a saved account to try first; the other saved
// organizations are tried only when the device proves it belongs elsewhere.
func (b *Backend) NearbyWiFiOpen(address, name, account string) (NearbyWiFiState, error) {
	n := &b.nearbyWiFi
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closeLocked()
	n.next++
	n.state = NearbyWiFiState{Session: n.next}
	if address == "" {
		return n.snapshot(), errors.New("Choose a device first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	certificates, err := nearbyWiFiCertificatesFn(ctx, account)
	if err != nil {
		return n.snapshot(), err
	}
	// The advertised name selects a saved device binding only (pinning); the
	// verified certificate supplies the identity.
	device := &models.BluetoothDevice{ID: address, Address: address, DisplayName: name, Name: name, IsWendyDevice: true, L2CAPPSM: ble.DefaultL2CAPPSM}
	var client nearbyAgent
	for _, cert := range certificates {
		client, err = nearbyWiFiConnectFn(device, cert)
		if err == nil {
			break
		}
		if !commands.DesktopBLEAgentOrganizationMismatch(err) {
			return n.snapshot(), err
		}
	}
	if client == nil {
		return n.snapshot(), errors.New("This device belongs to an organization you are not signed in to. Sign in to that organization, then try again.")
	}
	identity, ok := client.PeerIdentity()
	if !ok || identity.EntityType != certs.EntityAsset {
		client.Close()
		return n.snapshot(), errors.New("The device did not prove a Wendy device identity")
	}
	n.client = client
	n.state.Connected = true
	n.state.DeviceID, n.state.Principal = identity.EntityID, identity.IdentityKey()
	n.state.Organization = identity.TenantUUID
	if n.state.Organization == "" && identity.OrgID > 0 {
		n.state.Organization = strconv.Itoa(int(identity.OrgID))
	}
	if err := n.refreshWiFi(); err != nil {
		return n.snapshot(), err
	}
	return n.snapshot(), nil
}

func (b *Backend) NearbyWiFiClose(session uint64) {
	n := &b.nearbyWiFi
	n.mu.Lock()
	defer n.mu.Unlock()
	if session != 0 && n.state.Session != session {
		return
	}
	n.closeLocked()
}

// Actions: 0 status/keepalive, 1 scan Wi-Fi, 2 connect Wi-Fi, 3 disconnect
// Wi-Fi, 4 forget a saved network. The password is forwarded to the device
// once and never kept or reported.
func (b *Backend) NearbyWiFiAction(session uint64, action int, ssid, password string) (NearbyWiFiState, error) {
	n := &b.nearbyWiFi
	n.mu.Lock()
	defer n.mu.Unlock()
	if session == 0 || n.state.Session != session {
		return NearbyWiFiState{}, errors.New("This Wi-Fi session has ended")
	}
	if n.client == nil || !n.state.Connected {
		return n.snapshot(), errors.New("Bluetooth connection ended. Retry connection")
	}
	// A transport failure ends the session; a refusal the device reported
	// keeps it, so the user can correct the request.
	fail := func(err error) (NearbyWiFiState, error) {
		var refusal *ble.AgentError
		if errors.As(err, &refusal) {
			return n.snapshot(), err
		}
		n.closeLocked()
		return n.snapshot(), err
	}
	switch action {
	case 0:
		if err := n.refreshWiFi(); err != nil {
			return n.snapshot(), err
		}
	case 1:
		networks, err := n.client.WifiList()
		if err != nil {
			return fail(err)
		}
		n.state.Networks = nil
		for _, network := range networks {
			if network.GetSsid() != "" {
				n.state.Networks = append(n.state.Networks, network.GetSsid())
			}
		}
	case 2:
		if ssid == "" || len(ssid) > 32 || len(password) > 64 || strings.ContainsRune(ssid, 0) || strings.ContainsRune(password, 0) {
			return n.snapshot(), errors.New("Enter a valid network name and password")
		}
		if err := n.client.WifiConnect(ssid, password); err != nil {
			var refusal *ble.AgentError
			if errors.As(err, &refusal) {
				return n.snapshot(), errors.New("Wi-Fi did not connect. Check the network and password")
			}
			return fail(err)
		}
		if err := n.refreshWiFi(); err != nil {
			return n.snapshot(), err
		}
		if !n.state.WiFiConnected || n.state.SSID != ssid {
			return n.snapshot(), errors.New("The requested Wi-Fi connection was not confirmed")
		}
	case 3:
		if err := n.client.WifiDisconnect(); err != nil {
			return fail(err)
		}
		if err := n.refreshWiFi(); err != nil {
			return n.snapshot(), err
		}
	case 4:
		if ssid == "" {
			return n.snapshot(), errors.New("Choose a saved network to forget")
		}
		if err := n.client.WifiForget(ssid); err != nil {
			return fail(err)
		}
		if err := n.refreshWiFi(); err != nil {
			return n.snapshot(), err
		}
	default:
		return n.snapshot(), errors.New("Unknown Wi-Fi action")
	}
	return n.snapshot(), nil
}

// refreshWiFi reads the device's Wi-Fi status. A lost reply ends the session:
// a later reply could otherwise be taken for the answer to another command.
func (n *nearbyWiFi) refreshWiFi() error {
	status, err := n.client.WifiStatus()
	if err != nil {
		var refusal *ble.AgentError
		if !errors.As(err, &refusal) {
			n.closeLocked()
		}
		return err
	}
	n.state.WiFiConnected = status.GetConnected()
	n.state.SSID = status.GetSsid()
	return nil
}
