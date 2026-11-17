package mtls

import (
	"crypto/tls"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
)

// NewOperatorTLSConfig is the management-only counterpart of NewTLSConfig.
// The role gate runs on full and resumed handshakes. Peer identity comes from
// the verified leaf, never a command or the radio's discovery advertisement.
func NewOperatorTLSConfig(certPEM, chainPEM, keyPEM string, logger *zap.Logger, floor time.Time, mode interceptor.OrgMode) (*tls.Config, error) {
	expected, ok := certs.ScopeFromCertPEM(certPEM)
	if !ok || !expected.Known() {
		return nil, errors.New("operator management requires the device's tenant identity")
	}
	config, err := NewTLSConfig(certPEM, chainPEM, keyPEM, logger, floor)
	if err != nil {
		return nil, err
	}
	previous := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if previous != nil {
			if err := previous(state); err != nil {
				return err
			}
		}
		if len(state.PeerCertificates) == 0 {
			return errors.New("operator certificate required")
		}
		return interceptor.CheckOperatorCertificate(state.PeerCertificates[0], expected, mode)
	}
	return config, nil
}
