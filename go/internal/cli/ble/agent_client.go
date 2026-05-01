package ble

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

const (
	// WendyOS BLE agent L2CAP PSM
	wendyAgentL2CAPPSM = 128
)

// AgentClient communicates with a WendyOS agent over BLE L2CAP using
// protobuf-framed messages (UInt16 BE length prefix) over mTLS.
type AgentClient struct {
	conn    *Connection
	tlsConn *tls.Conn
}

// ConnectAgent establishes a BLE connection to a WendyOS device, opens the
// L2CAP channel, and performs the mTLS handshake. tlsConfig must include a
// client certificate issued by the same PKI as the agent's server certificate.
func ConnectAgent(device *models.BluetoothDevice, tlsConfig *tls.Config) (*AgentClient, error) {
	conn, err := Connect(device.Address, 10)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", device.DisplayName, err)
	}

	psm := uint16(wendyAgentL2CAPPSM)
	if device.L2CAPPSM != 0 {
		psm = device.L2CAPPSM
	}

	if err := conn.OpenL2CAP(psm, 10); err != nil {
		conn.Close()
		return nil, fmt.Errorf("opening L2CAP channel (PSM %d): %w", psm, err)
	}

	tlsConn := tls.Client(newL2CAPNetConn(conn), tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("BLE mTLS handshake: %w", err)
	}

	return &AgentClient{conn: conn, tlsConn: tlsConn}, nil
}

// Close disconnects the BLE connection.
func (c *AgentClient) Close() {
	c.tlsConn.Close()
}

// sendCommand serializes a BluetoothCommand, sends it over the mTLS stream
// with a UInt16 BE length prefix, reads the response, and returns it.
func (c *AgentClient) sendCommand(cmd *agentpb.BluetoothCommand) (*agentpb.BluetoothResponse, error) {
	data, err := proto.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshaling command: %w", err)
	}

	// Build length-prefixed frame: [UInt16 BE length] [protobuf data]
	frame := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(data)))
	copy(frame[2:], data)

	if _, err := c.tlsConn.Write(frame); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}

	// Read the 2-byte length header, then the body.
	var header [2]byte
	if _, err := io.ReadFull(c.tlsConn, header[:]); err != nil {
		return nil, fmt.Errorf("reading response header: %w", err)
	}
	msgLen := binary.BigEndian.Uint16(header[:])
	body := make([]byte, msgLen)
	if _, err := io.ReadFull(c.tlsConn, body); err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	resp := &agentpb.BluetoothResponse{}
	if err := proto.Unmarshal(body, resp); err != nil {
		return nil, fmt.Errorf("unmarshaling response: %w", err)
	}

	if errResp := resp.GetError(); errResp != nil {
		return nil, fmt.Errorf("agent error: %s", errResp.GetMessage())
	}

	return resp, nil
}

// WifiConnect sends a WiFi connect command over BLE.
func (c *AgentClient) WifiConnect(ssid, password string) error {
	return c.WifiConnectWith(ssid, password, agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED, false)
}

// WifiConnectWith sends a WiFi connect command over BLE with optional security
// hint and hidden-network flag.
func (c *AgentClient) WifiConnectWith(ssid, password string, security agentpb.WiFiSecurityType, hidden bool) error {
	inner := &agentpb.WifiConnectCommand{
		Ssid:     ssid,
		Password: password,
	}
	if security != agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED {
		s := security
		inner.Security = &s
	}
	if hidden {
		h := true
		inner.Hidden = &h
	}
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiConnect{
			WifiConnect: inner,
		},
	}

	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}

	wifiResp := resp.GetWifiConnect()
	if wifiResp == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !wifiResp.GetSuccess() {
		msg := wifiResp.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("WiFi connect failed: %s", msg)
	}

	return nil
}

// WifiList lists available WiFi networks over BLE.
func (c *AgentClient) WifiList() ([]*agentpb.WifiNetworkInfo, error) {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiList{
			WifiList: &agentpb.WifiListCommand{},
		},
	}

	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}

	wifiResp := resp.GetWifiList()
	if wifiResp == nil {
		return nil, fmt.Errorf("unexpected response type")
	}
	return wifiResp.GetNetworks(), nil
}

// WifiStatus gets the current WiFi connection status over BLE.
func (c *AgentClient) WifiStatus() (*agentpb.WifiStatusResponse, error) {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiStatus{
			WifiStatus: &agentpb.WifiStatusCommand{},
		},
	}

	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}

	wifiResp := resp.GetWifiStatus()
	if wifiResp == nil {
		return nil, fmt.Errorf("unexpected response type")
	}
	return wifiResp, nil
}

// WifiDisconnect disconnects from the current WiFi network over BLE.
func (c *AgentClient) WifiDisconnect() error {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiDisconnect{
			WifiDisconnect: &agentpb.WifiDisconnectCommand{},
		},
	}

	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}

	wifiResp := resp.GetWifiDisconnect()
	if wifiResp == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !wifiResp.GetSuccess() {
		msg := wifiResp.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("WiFi disconnect failed: %s", msg)
	}
	return nil
}

// WifiKnownList lists saved WiFi profiles on the device over BLE.
func (c *AgentClient) WifiKnownList() ([]*agentpb.KnownWifiNetworkInfo, error) {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiKnownList{
			WifiKnownList: &agentpb.WifiKnownListCommand{},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}
	inner := resp.GetWifiKnownList()
	if inner == nil {
		return nil, fmt.Errorf("unexpected response type")
	}
	return inner.GetNetworks(), nil
}

// WifiSetPriority sets the priority for a saved network by SSID.
func (c *AgentClient) WifiSetPriority(ssid string, priority int32) error {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiSetPriority{
			WifiSetPriority: &agentpb.WifiSetPriorityCommand{
				Ssid:     ssid,
				Priority: priority,
			},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}
	inner := resp.GetWifiSetPriority()
	if inner == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !inner.GetSuccess() {
		msg := inner.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("WiFi set priority failed: %s", msg)
	}
	return nil
}

// WifiReorder reorders saved networks by SSID.
func (c *AgentClient) WifiReorder(order []string) error {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiReorder{
			WifiReorder: &agentpb.WifiReorderCommand{OrderSsids: order},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}
	inner := resp.GetWifiReorder()
	if inner == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !inner.GetSuccess() {
		msg := inner.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("WiFi reorder failed: %s", msg)
	}
	return nil
}

// AgentVersion returns the agent version and device info over BLE.
func (c *AgentClient) AgentVersion() (*agentpb.AgentVersionResponse, error) {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AgentVersion{
			AgentVersion: &agentpb.AgentVersionCommand{},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}
	inner := resp.GetAgentVersion()
	if inner == nil {
		return nil, fmt.Errorf("unexpected response type")
	}
	return inner, nil
}

// AppsList returns the list of deployed apps over BLE.
func (c *AgentClient) AppsList() ([]*agentpb.AppInfo, error) {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsList{
			AppsList: &agentpb.AppsListCommand{},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}
	inner := resp.GetAppsList()
	if inner == nil {
		return nil, fmt.Errorf("unexpected response type")
	}
	return inner.GetApps(), nil
}

// AppsStop stops an app by name over BLE.
func (c *AgentClient) AppsStop(appName string) error {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsStop{
			AppsStop: &agentpb.AppsStopCommand{AppName: appName},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}
	inner := resp.GetAppsStop()
	if inner == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !inner.GetSuccess() {
		msg := inner.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("apps stop failed: %s", msg)
	}
	return nil
}

// AppsRemove removes an app over BLE. purgeImage removes the container image.
func (c *AgentClient) AppsRemove(appName string, purgeImage bool) error {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_AppsRemove{
			AppsRemove: &agentpb.AppsRemoveCommand{
				AppName:    appName,
				PurgeImage: purgeImage,
			},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}
	inner := resp.GetAppsRemove()
	if inner == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !inner.GetSuccess() {
		msg := inner.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("apps remove failed: %s", msg)
	}
	return nil
}

// HardwareList returns hardware capabilities over BLE.
func (c *AgentClient) HardwareList() ([]*agentpb.HardwareInfo, error) {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_HardwareList{
			HardwareList: &agentpb.HardwareListCommand{},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}
	inner := resp.GetHardwareList()
	if inner == nil {
		return nil, fmt.Errorf("unexpected response type")
	}
	return inner.GetCapabilities(), nil
}

// WifiForget removes a saved WiFi profile by SSID.
func (c *AgentClient) WifiForget(ssid string) error {
	cmd := &agentpb.BluetoothCommand{
		Command: &agentpb.BluetoothCommand_WifiForget{
			WifiForget: &agentpb.WifiForgetCommand{Ssid: ssid},
		},
	}
	resp, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}
	inner := resp.GetWifiForget()
	if inner == nil {
		return fmt.Errorf("unexpected response type")
	}
	if !inner.GetSuccess() {
		msg := inner.GetErrorMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("WiFi forget failed: %s", msg)
	}
	return nil
}
