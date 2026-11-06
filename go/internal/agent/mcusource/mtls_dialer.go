package mcusource

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
)

// Identity returns this agent's own mTLS asset identity (cert/chain/key
// PEMs), read fresh on every call so a certificate rotated or issued while
// the agent is running (BLE/OnProvisioned) is picked up without a restart —
// the same freshness contract as the PushTLS closure in cmd/wendy-agent.
type Identity func() (certPEM, chainPEM, keyPEM string)

// wantPeerIdentity resolves the identity a pairing's source must present on
// the handshake: the paired device principal when the pairing carries one,
// or the legacy numeric asset id pinned inside this agent's own tenant —
// the same contract the pre-v2 dialer enforced for asset-id pairings.
func wantPeerIdentity(p SensorPairing, ownCertPEM string) (certs.WendyIdentity, error) {
	if p.SourcePrincipalURI != "" {
		id, err := certs.ParseDevicePrincipal(p.SourcePrincipalURI)
		if err != nil {
			return certs.WendyIdentity{}, fmt.Errorf("mcusource: invalid source principal: %w", err)
		}
		return id, nil
	}
	if p.SourceAssetID <= 0 {
		return certs.WendyIdentity{}, errors.New("mcusource: pairing carries no source identity")
	}
	scope, _ := certs.ScopeFromCertPEM(ownCertPEM)
	return certs.WendyIdentity{
		OrgID:      scope.OrgID,
		TenantUUID: scope.TenantUUID,
		EntityType: certs.EntityAsset,
		EntityID:   strconv.Itoa(int(p.SourceAssetID)),
	}, nil
}

// mtlsDialer opens a TCP+TLS connection to one sensor source, pinning the
// peer's identity (paired principal, or legacy asset id in own tenant) on
// the handshake itself — the same per-target pinning meshDialLAN uses for
// mesh peers.
type mtlsDialer struct {
	logger   *zap.Logger
	certPEM  string
	chainPEM string
	keyPEM   string
	want     certs.WendyIdentity
}

func (d mtlsDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	tlsCfg, err := mtls.NewClientTLSConfigExpectingIdentity(d.certPEM, d.chainPEM, d.keyPEM, d.logger, d.want)
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
// identity comes from the pairing itself. Sensor pairing is same-tenant by
// design.
func NewMTLSDialer(logger *zap.Logger, identity Identity) func(SensorPairing) (Dialer, error) {
	return func(p SensorPairing) (Dialer, error) {
		certPEM, chainPEM, keyPEM := identity()
		if certPEM == "" || keyPEM == "" {
			return nil, fmt.Errorf("mcusource: agent has no mTLS identity (not provisioned)")
		}
		want, err := wantPeerIdentity(p, certPEM)
		if err != nil {
			return nil, err
		}
		return mtlsDialer{
			logger:   logger,
			certPEM:  certPEM,
			chainPEM: chainPEM,
			keyPEM:   keyPEM,
			want:     want,
		}, nil
	}
}
