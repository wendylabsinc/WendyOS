package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
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

// DirectNativeDialer is an authenticated session owner, not a raw socket
// dialer. Implementations must bind the returned certificate to the exact
// retained stream epoch and revoke I/O when its authority expires or changes.
// meshsession.DirectManager implements this contract.
type DirectNativeDialer interface {
	DialNative(context.Context, meshid.Device, byte) (net.Conn, *x509.Certificate, error)
}

// ConnectDirectContext opens WendyCom on an already authenticated retained BLE
// epoch. The manager enforces identity and credential lifetime through carrier
// loss/replacement. Only ErrNoDirectSession permits the caller to try another
// transport; an existing epoch owns pending and failed opens.
func (c *WendyLiteClient) ConnectDirectContext(ctx context.Context, manager DirectNativeDialer, peer meshid.Device, pins certs.PinChecker) error {
	if manager == nil || !peer.IsValid() || peer.IsOperator() {
		return errors.New("missing direct device session")
	}
	conn, leaf, err := manager.DialNative(ctx, peer, directstream.ServiceWendyCom)
	if err != nil {
		return err
	}
	if leaf == nil {
		conn.Close()
		return errors.New("direct native stream has no authenticated certificate")
	}
	if pins != nil {
		if err := pins.CheckAndUpdate(leaf, leaf.Subject.CommonName); err != nil {
			var blocking certs.BlockingPinError
			if errors.As(err, &blocking) {
				conn.Close()
				return err
			}
		}
	}
	return c.connectAuthenticatedStreamContext(ctx, conn, leaf)
}
