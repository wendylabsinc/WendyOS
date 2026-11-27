package localmesh

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// TCPConfig retains explicit TCP simulation links and independently enables
// discovered wendy net carriers. Each explicit edge is configured at both ends.
type TCPConfig struct {
	Listen string    `json:"listen" cbor:"l"`
	Peers  []TCPPeer `json:"peers" cbor:"p"`
	NAN    bool      `json:"nan,omitempty" cbor:"n,omitempty"`
	BLE    bool      `json:"ble,omitempty" cbor:"b,omitempty"`
	// TLSListen enables native TLS packet attachments without per-device configuration.
	TLSListen string `json:"tls_listen,omitempty" cbor:"e,omitempty"`

	// devices holds the parsed peer identities, in the same order as Peers.
	devices            []meshid.Device
	Ethernet           bool `json:"ethernet,omitempty" cbor:"E,omitempty"`
	InfrastructureWiFi bool `json:"infrastructureWifi,omitempty" cbor:"i,omitempty"`
}

// TCPPeer names a configured neighbour by its device UUID - the same 16 bytes
// the mesh address and link-local ordering are derived from. There is no asset
// number and no organization number: the tenant comes from this device's own
// verified certificate.
type TCPPeer struct {
	Device  string `json:"device" cbor:"d"`
	Address string `json:"address" cbor:"a"`
}

type TCPIdentity struct {
	// TimePolicy is runtime-only and applies solely to mesh carrier credentials.
	TimePolicy              *certs.CertificateTimePolicy `json:"-" cbor:"-"`
	Name                    string
	AgentPort               uint16
	AppEndpoints            []AppEndpoint // supplied only for ready local listeners
	Certificate, Chain, Key string
	Agent                   *AgentMetadata
	// PreparedCredentials carries the provisioning snapshot shared with the
	// app sessions. Configured carriers should use the same verified CA set.
	PreparedCredentials *Credentials `json:"-"`
}

func (id TCPIdentity) MeshCredentials() (*Credentials, error) {
	if id.PreparedCredentials != nil {
		return id.PreparedCredentials, nil
	}
	if id.TimePolicy != nil {
		return NewCredentialsWithTimePolicy(id.Certificate, id.Chain, id.Key, *id.TimePolicy)
	}
	return NewCredentials(id.Certificate, id.Chain, id.Key)
}

// Devices returns the parsed peers of a config loaded for self.
func (c *TCPConfig) Devices() []meshid.Device { return c.devices }

// ReadTCPConfigFile reads a bounded snapshot using the configuration loader's
// opener. On Linux this also rejects symlinks and special or unprotected files,
// so watchers cannot block on FIFOs or bypass the privileged loader's checks.
func ReadTCPConfigFile(path string) ([]byte, error) {
	f, err := openTCPConfig(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 65536 {
		return nil, errors.New("wendy net config size out of range")
	}
	return data, nil
}

// DisabledCarrierConfigPath returns the path of the all-carriers-off marker.
func DisabledCarrierConfigPath(path string) string { return path + ".disabled" }

// ReadTCPConfigSnapshot returns the effective carrier document. A nil
// snapshot means the administrator explicitly disabled all carriers.
func ReadTCPConfigSnapshot(path string) ([]byte, error) {
	data, err := ReadTCPConfigFile(path)
	if !errors.Is(err, os.ErrNotExist) {
		return data, err
	}
	override, err := ReadTCPConfigFile(DisabledCarrierConfigPath(path))
	if errors.Is(err, os.ErrNotExist) {
		data, err := json.Marshal(TCPConfig{Ethernet: true, InfrastructureWiFi: true, BLE: true, NAN: true})
		return append(data, '\n'), err
	}
	if err != nil {
		return nil, err
	}
	if string(override) != "disabled\n" {
		return nil, errors.New("invalid Wendy Net carrier disable override")
	}
	return nil, nil
}

func LoadTCPConfig(path string, self meshid.Device) (*TCPConfig, error) {
	data, err := ReadTCPConfigSnapshot(path)
	if err != nil {
		return nil, err
	}
	return ParseTCPConfig(data, self)
}

// ParseTCPConfig validates an effective snapshot for this device.
func ParseTCPConfig(data []byte, self meshid.Device) (*TCPConfig, error) {
	if data == nil {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// The disk format accepts leaf-only fields. They do not enable a carrier
	// and are omitted when settings are saved.
	var stored struct {
		TCPConfig
		LeafListen   json.RawMessage `json:"leafListen"`
		LeafBLEPeers json.RawMessage `json:"leafBLEPeers"`
		LeafTimeURL  json.RawMessage `json:"leafTimeURL"`
	}
	if err := dec.Decode(&stored); err != nil {
		return nil, fmt.Errorf("wendy net config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("wendy net config has trailing data")
	}
	cfg := stored.TCPConfig
	retired := len(stored.LeafListen) != 0 || len(stored.LeafBLEPeers) != 0 || len(stored.LeafTimeURL) != 0
	hasCarrier := cfg.NAN || cfg.BLE || cfg.Ethernet || cfg.InfrastructureWiFi || cfg.TLSListen != ""
	if retired && !hasCarrier && cfg.Listen == "" && len(cfg.Peers) == 0 {
		return nil, nil
	}
	if cfg.Listen == "" {
		if !hasCarrier || len(cfg.Peers) != 0 {
			return nil, errors.New("listen is required for configured TCP peers; enable a carrier for discovery-only networking")
		}
	} else if err := validateTCPAddress(cfg.Listen, true); err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if cfg.TLSListen != "" {
		if err := validateTCPAddress(cfg.TLSListen, true); err != nil {
			return nil, fmt.Errorf("tls_listen: %w", err)
		}
		if cfg.TLSListen == cfg.Listen {
			return nil, errors.New("tls_listen must use a separate listener")
		}
		host, port, _ := net.SplitHostPort(cfg.TLSListen)
		ip := net.ParseIP(host)
		portNumber, _ := strconv.Atoi(port)
		if (cfg.Ethernet || cfg.InfrastructureWiFi) && portNumber == TLSLANPort &&
			(host == "" || (ip != nil && ip.IsUnspecified())) {
			return nil, errors.New("tls_listen wildcard port 43024 conflicts with automatic LAN listeners; use another port or disable LAN discovery")
		}
	}
	if len(cfg.Peers) > 64 {
		return nil, errors.New("too many configured wendy net peers")
	}
	seen := make(map[string]bool, len(cfg.Peers))
	cfg.devices = make([]meshid.Device, 0, len(cfg.Peers))
	for _, p := range cfg.Peers {
		device, err := meshid.ParseDevice(self.Tenant(), p.Device)
		if err != nil {
			return nil, fmt.Errorf("wendy net peer: %w", err)
		}
		if device.Principal() == self.Principal() || seen[device.Principal()] {
			return nil, fmt.Errorf("invalid or duplicate wendy net peer device %s", p.Device)
		}
		seen[device.Principal()] = true
		if err := validateTCPAddress(p.Address, false); err != nil {
			return nil, fmt.Errorf("peer %s: %w", p.Device, err)
		}
		cfg.devices = append(cfg.devices, device)
	}
	return &cfg, nil
}

func validateTCPAddress(address string, listen bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be 1..65535")
	}
	if !listen && (host == "" || host == "0.0.0.0" || host == "::") {
		return errors.New("peer needs a concrete host")
	}
	return nil
}
