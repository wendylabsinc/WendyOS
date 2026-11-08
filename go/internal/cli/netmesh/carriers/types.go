// Package carriers discovers and maintains the bounded physical links of one
// broker-owned mesh node. Advertisements are scheduling hints; Connect owns TLS
// authentication, peer admission and the complete attached link lifetime.
package carriers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// ErrUnsupportedPeer is a normal negotiated capability mismatch, not an authentication fallback.
var ErrUnsupportedPeer = errors.New("peer does not support primary operator-router carriers")

const (
	TCP  = "tcp"
	QUIC = "quic"
	BLE  = "ble"
)

// Candidate is an unauthenticated discovery hint. Device may be zero when BLE
// supplies only a short UUID hint. It must never be synthesized from that hint.
type Candidate struct {
	Device        meshid.Device
	Address       string
	Transport     string
	Cost          uint16
	PSM           uint16
	Hint          [4]byte
	NetworkHash   [2]byte
	Capabilities  byte
	InterfaceName string
	authenticated func(meshid.Device)
}

// Authenticated records the exact peer ONLY after the caller has verified the
// primary TLS identity and any advertised identity/installation binding. Call
// before rejecting a redundant path so a known BLE radio need not repeatedly
// authenticate while a cheaper LAN path remains healthy.
func (c Candidate) Authenticated(peer meshid.Device) {
	if c.authenticated != nil {
		c.authenticated(peer)
	}
}

func (c Candidate) key() string { return fmt.Sprintf("%s/%s/%d", c.Transport, c.Address, c.PSM) }
func (c Candidate) valid() bool {
	if c.Address == "" || len(c.Address) > 512 || c.Cost == 0 {
		return false
	}
	switch c.Transport {
	case TCP, QUIC:
		_, port, err := net.SplitHostPort(c.Address)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(port)
		return err == nil && n > 0 && n <= 65535
	case BLE:
		return c.PSM >= 0x80 && c.PSM <= 0xff
	}
	return false
}
func (c Candidate) sameAdvertisement(d Candidate) bool {
	return c.Device == d.Device && c.Address == d.Address && c.Transport == d.Transport && c.PSM == d.PSM && c.Hint == d.Hint && c.NetworkHash == d.NetworkHash && c.Capabilities == d.Capabilities
}

// Source emits complete fresh snapshots, not an accumulating roster. A source
// must honor cancellation and bound the number of retained observations.
type Source func(context.Context, func([]Candidate)) error

type Limits struct {
	Candidates       int
	Connections      int
	Attempts         int
	BLEConnections   int
	RetryDelay       time.Duration
	CandidateTTL     time.Duration
	AuthenticatedTTL time.Duration
	PollInterval     time.Duration
}

// Config supplies the already-running node's authenticated view and attachment
// callback. A nil Sources slice selects native discovery; a non-nil empty slice
// intentionally runs no discovery. By default TCP and BLE are usable; callers
// explicitly add QUIC once their operator carrier implements it.
type Config struct {
	Credentials *localmesh.Credentials
	Snapshot    func() localmesh.NodeSnapshot
	Connect     func(context.Context, Candidate) error
	// Seeds are persistent first-hop candidates, bounded by Limits.Candidates.
	// Fresh discovery may update their metadata without removing reconnect intent.
	Seeds               []Candidate
	Sources             []Source
	SupportedTransports []string
	Limits              Limits
	Status              func(error)
}

func (c *Config) defaults() error {
	if c.Credentials == nil || !c.Credentials.Device.Tenant().IsValid() || c.Connect == nil {
		return errors.New("carrier discovery requires authenticated mesh credentials and an attachment callback")
	}
	l := &c.Limits
	if l.Candidates == 0 {
		l.Candidates = 256
	}
	if l.Connections == 0 {
		l.Connections = 8
	}
	if l.Attempts == 0 {
		l.Attempts = 2
	}
	if l.BLEConnections == 0 {
		l.BLEConnections = 2
	}
	if l.Candidates < 1 || l.Candidates > 1024 || l.Connections < 1 || l.Connections > 64 || l.Attempts < 1 || l.Attempts > l.Connections || l.BLEConnections < 0 || l.BLEConnections > l.Connections {
		return errors.New("invalid bounded carrier limits")
	}
	if l.RetryDelay == 0 {
		l.RetryDelay = 5 * time.Second
	}
	if l.CandidateTTL == 0 {
		l.CandidateTTL = 30 * time.Second
	}
	if l.AuthenticatedTTL == 0 {
		l.AuthenticatedTTL = time.Minute
	}
	if l.PollInterval == 0 {
		l.PollInterval = 250 * time.Millisecond
	}
	if l.RetryDelay < time.Millisecond || l.CandidateTTL <= 0 || l.AuthenticatedTTL <= 0 || l.PollInterval < time.Millisecond {
		return errors.New("invalid carrier timing")
	}
	if len(c.Seeds) > l.Candidates {
		return errors.New("seed candidates exceed carrier candidate limit")
	}
	for _, seed := range c.Seeds {
		if !seed.valid() || (seed.Device != (meshid.Device{}) && seed.Device.Tenant() != c.Credentials.Device.Tenant()) {
			return errors.New("invalid seed candidate")
		}
	}
	if len(c.SupportedTransports) == 0 {
		c.SupportedTransports = []string{TCP, BLE}
	}
	for _, s := range c.SupportedTransports {
		if s != TCP && s != QUIC && s != BLE {
			return errors.New("unsupported carrier transport")
		}
	}
	return nil
}
