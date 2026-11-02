package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/central"
)

// bleDialTimeout covers each step of reaching a device — GAP connect, service
// discovery, opening the L2CAP channel, the TLS handshake — rather than the
// whole sequence. A cold connect to an advertising device is well under a
// second; this is the give-up point, not the expectation.
const bleDialTimeout = 10 * time.Second

// DefaultL2CAPPSM is the PSM the Wendy Lite firmware listens on. It is the last
// fallback in ConnectViaBLE: used when the caller passed 0 and the device's GATT
// info service could not be read.
const DefaultL2CAPPSM uint16 = 128

// ConnectViaBLE reaches a device over BLE: it opens the L2CAP channel, runs
// the TLS handshake on it, and then speaks ordinary WendyCom. Everything above
// the byte stream is identical to the TCP path.
//
// psm may be 0, in which case it comes from the device's GATT info service,
// falling back to DefaultL2CAPPSM where GATT is unavailable.
//
// tlsCfg carries the client's side of the trust decision; nil selects the
// insecure configuration an unprovisioned device needs.
func (c *WendyLiteClient) ConnectViaBLE(address string, psm uint16, tlsCfg *tls.Config) error {
	return c.ConnectViaBLEContext(context.Background(), address, psm, tlsCfg)
}

// ConnectViaBLEContext cancels connection setup and both protocol handshakes.
func (c *WendyLiteClient) ConnectViaBLEContext(ctx context.Context, address string, psm uint16, tlsCfg *tls.Config) error {
	stream, err := dialBLEStream(ctx, address, psm)
	if err != nil {
		return err
	}
	stopCancel := context.AfterFunc(ctx, func() { stream.Close() })
	defer stopCancel()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec -- unprovisioned devices serve a self-signed cert
			MinVersion:         tls.VersionTLS12,
		}
	}
	// Certificate flights have the same finite radio budget as a mesh TCP
	// endpoint; GAP/GATT/L2CAP setup retain their shorter independent limits.
	_ = stream.SetDeadline(time.Now().Add(60 * time.Second))
	tlsConn := tls.Client(stream, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		stream.Close() //nolint:errcheck,gosec -- teardown on an already-failed path
		return fmt.Errorf("BLE TLS handshake: %w", err)
	}
	_ = stream.SetDeadline(time.Time{})
	c.link = newDirectLink(tlsConn)
	if err := c.handshakeWithin(60 * time.Second); err != nil {
		c.link.close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	if tlsCfg.VerifyConnection != nil {
		c.peerCert = tlsConn.ConnectionState().PeerCertificates[0]
	}
	c.startReadLoop()
	return nil
}

// The native BLE API has bounded blocking calls but no context support. Keep
// its connection local until setup finishes, and close it if cancellation wins.
func dialBLEStream(ctx context.Context, address string, psm uint16) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		stream net.Conn
		err    error
	}
	completed := make(chan result)
	open := openBLEStreamFn
	go func() {
		stream, err := open(ctx, address, psm)
		select {
		case completed <- result{stream: stream, err: err}:
		case <-ctx.Done():
			if stream != nil {
				stream.Close()
			}
		}
	}()
	select {
	case result := <-completed:
		return result.stream, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var openBLEStreamFn = openBLEStream

func openBLEStream(ctx context.Context, address string, psm uint16) (net.Conn, error) {
	// The BLE client takes whole seconds, not a Duration; bleDialTimeout is a
	// whole number of them.
	timeoutSecs := int(bleDialTimeout / time.Second)

	conn, err := central.Connect(address, timeoutSecs)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}

	if psm == 0 {
		// Not fatal: where GATT is unavailable — Windows has no GATT client at
		// all, and a board may simply not publish the service — the default is
		// the answer, and a device that really disagrees fails the open below
		// with a clearer error than this would give.
		if info, ierr := ble.ReadLiteInfo(conn, bleDialTimeout); ierr == nil {
			psm = info.PSM
		}
		if psm == 0 {
			psm = DefaultL2CAPPSM
		}
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}

	if err := conn.OpenL2CAP(psm, timeoutSecs); err != nil {
		conn.Close()
		return nil, fmt.Errorf("opening L2CAP channel (PSM %d): %w", psm, err)
	}
	return central.NewL2CAPStream(conn), nil
}

// ConnectViaBLEWithPKIAuthentication verifies the Wendy-issued chain and the
// selected device identity with the same verifier used by LAN and Cloud.
func (c *WendyLiteClient) ConnectViaBLEWithPKIAuthentication(address string, psm uint16, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) error {
	return c.ConnectViaBLEWithPKIAuthenticationContext(context.Background(), address, psm, cert, chain, deviceID, lite)
}

func (c *WendyLiteClient) ConnectViaBLEWithPKIAuthenticationContext(ctx context.Context, address string, psm uint16, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) error {
	cfg, err := deviceTLSConfig(ctx, cert, chain, deviceID, lite)
	if err != nil {
		return err
	}
	return c.ConnectViaBLEContext(ctx, address, psm, cfg)
}

// ConnectViaBLEInsecure reaches a device over BLE without verifying its
// certificate, mirroring ConnectInsecure on TCP. An unprovisioned device
// serves the self-signed certificate compiled into the firmware, which
// authenticates nothing — and reaching an unprovisioned device is most of what
// BLE is for.
//
// SECURITY: development use only. Prefer
// ConnectViaBLEWithMutualAuthentication against a provisioned device.
func (c *WendyLiteClient) ConnectViaBLEInsecure(address string, psm uint16) error {
	return c.ConnectViaBLE(address, psm, nil)
}

// ConnectViaBLEWithMutualAuthentication reaches a provisioned device over BLE,
// presenting a client certificate and verifying the device's against rootCAs.
//
// Hostname verification is off because there is no hostname over L2CAP; the
// explicit chain check is what establishes identity. This mirrors
// ConnectWithMutualAuthentication on TCP.
func (c *WendyLiteClient) ConnectViaBLEWithMutualAuthentication(
	address string, psm uint16, cert tls.Certificate, rootCAs x509.CertPool) error {

	var verifiedLeaf *x509.Certificate
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec — hostname bypass only; the chain is checked below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, len(rawCerts))
			for i, raw := range rawCerts {
				parsed, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parsing device certificate: %w", err)
				}
				certs[i] = parsed
			}
			opts := x509.VerifyOptions{
				Roots:         &rootCAs,
				Intermediates: x509.NewCertPool(),
			}
			for _, intermediate := range certs[1:] {
				opts.Intermediates.AddCert(intermediate)
			}
			if _, err := certs[0].Verify(opts); err != nil {
				return fmt.Errorf("device certificate verification failed: %w", err)
			}
			verifiedLeaf = certs[0]
			return nil
		},
	}
	if err := c.ConnectViaBLE(address, psm, tlsCfg); err != nil {
		return err
	}
	c.peerCert = verifiedLeaf
	return nil
}
