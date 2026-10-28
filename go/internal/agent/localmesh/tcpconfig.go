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
	Name                    string
	AgentPort               uint16
	AppEndpoints            []AppEndpoint // supplied only for ready local listeners
	Certificate, Chain, Key string
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

func LoadTCPConfig(path string, self meshid.Device) (*TCPConfig, error) {
	data, err := ReadTCPConfigFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg TCPConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("wendy net config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("wendy net config has trailing data")
	}
	if cfg.Listen == "" {
		if (!cfg.NAN && !cfg.BLE && !cfg.Ethernet && !cfg.InfrastructureWiFi && cfg.TLSListen == "") || len(cfg.Peers) != 0 {
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
