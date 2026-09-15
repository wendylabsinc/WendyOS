package mcusource

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"go.uber.org/zap"
)

// Identity returns this agent's own mTLS asset identity (cert/chain/key
// PEMs), read fresh on every call so a certificate rotated or issued while
// the agent is running (BLE/OnProvisioned) is picked up without a restart —
// the same freshness contract as the PushTLS closure in cmd/wendy-agent.
type Identity func() (certPEM, chainPEM, keyPEM string)

// mtlsDialer opens a TCP+TLS connection to one sensor source, pinning the
// peer's asset identity (org + source asset id) on the handshake itself —
// the same per-target pinning meshDialLAN uses for mesh peers.
type mtlsDialer struct {
	logger   *zap.Logger
	certPEM  string
	chainPEM string
	keyPEM   string
	orgID    int32
	assetID  int32
}

func (d mtlsDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	tlsCfg, err := mtls.NewClientTLSConfigExpectingPeer(d.certPEM, d.chainPEM, d.keyPEM, d.logger,
		d.orgID, strconv.Itoa(int(d.assetID)))
	if err != nil {
		return nil, fmt.Errorf("mcusource: client TLS config: %w", err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcusource: dial %s: %w", addr, err)
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mcusource: TLS handshake with %s: %w", addr, err)
	}
	return tlsConn, nil
}

// NewMTLSDialer returns a per-pairing Dialer factory: identity supplies this
// agent's own credentials (read fresh per call), while the expected peer
// identity (org + asset id) comes from the pairing itself. Sensor pairing is
// same-org by design — SensorPairing.OrgID is always set to the agent's own
// org (see sensor_pairing_service.go) — so this pins the handshake to the
// source's asset id within that same org, never a different one.
func NewMTLSDialer(logger *zap.Logger, identity Identity) func(SensorPairing) (Dialer, error) {
	return func(p SensorPairing) (Dialer, error) {
		certPEM, chainPEM, keyPEM := identity()
		if certPEM == "" || keyPEM == "" {
			return nil, fmt.Errorf("mcusource: agent has no mTLS identity (not provisioned)")
		}
		return mtlsDialer{
			logger:   logger,
			certPEM:  certPEM,
			chainPEM: chainPEM,
			keyPEM:   keyPEM,
			orgID:    p.OrgID,
			assetID:  p.SourceAssetID,
		}, nil
	}
}
