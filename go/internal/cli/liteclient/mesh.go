package liteclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// ConnectWithPKIDialerContext runs the normal device mTLS and WendyCom
// handshake over a caller-owned routed TCP dialer. The dialer supplies bytes;
// it does not terminate another TLS layer or change either authenticated role.
func (c *WendyLiteClient) ConnectWithPKIDialerContext(ctx context.Context, address string, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain, pins certs.PinChecker, dial func(context.Context, string, string) (net.Conn, error)) error {
	cfg, err := deviceTLSConfig(ctx, cert, chain, deviceID, lite)
	if err != nil {
		return err
	}
	if pins != nil {
		verify := cfg.VerifyConnection
		cfg.VerifyConnection = func(state tls.ConnectionState) error {
			if err := verify(state); err != nil {
				return err
			}
			// Pin only after the final chain/identity verifier, including the existing
			// Lite Device CA fallback, has accepted this exact device.
			leaf := state.PeerCertificates[0]
			if err := pins.CheckAndUpdate(leaf, leaf.Subject.CommonName); err != nil {
				var blocking certs.BlockingPinError
				if errors.As(err, &blocking) {
					return err
				}
			}
			return nil
		}
	}
	return c.connectTLSWithDialerContext(ctx, address, cfg, dial)
}
