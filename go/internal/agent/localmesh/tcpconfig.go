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
)

// TCPConfig is an opt-in, explicitly provisioned topology for local agent
// simulations. Each edge must be configured at both endpoints.
type TCPConfig struct {
	Listen string    `json:"listen"`
	Peers  []TCPPeer `json:"peers"`
	NAN    bool      `json:"nan,omitempty"`
}

type TCPPeer struct {
	Asset   int32  `json:"asset"`
	Address string `json:"address"`
}

type TCPIdentity struct {
	Org, Asset              int32
	Name                    string
	AgentPort               uint16
	Certificate, Chain, Key string
}

func LoadTCPConfig(path string, self int32) (*TCPConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 65536 {
		return nil, errors.New("local mesh config size out of range")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg TCPConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("local mesh config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("local mesh config has trailing data")
	}
	if cfg.Listen == "" {
		if !cfg.NAN || len(cfg.Peers) != 0 {
			return nil, errors.New("listen is required for configured TCP peers; set nan to enable radio-only mesh")
		}
	} else if err := validateTCPAddress(cfg.Listen, true); err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if len(cfg.Peers) > 64 {
		return nil, errors.New("too many configured mesh peers")
	}
	seen := make(map[int32]bool, len(cfg.Peers))
	for _, p := range cfg.Peers {
		if p.Asset <= 0 || p.Asset > 65534 || p.Asset == self || seen[p.Asset] {
			return nil, fmt.Errorf("invalid or duplicate mesh peer asset %d", p.Asset)
		}
		seen[p.Asset] = true
		if err := validateTCPAddress(p.Address, false); err != nil {
			return nil, fmt.Errorf("peer %d: %w", p.Asset, err)
		}
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
