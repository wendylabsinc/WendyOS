package liteclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
	// The BLE client takes whole seconds, not a Duration; bleDialTimeout is a
	// whole number of them.
	timeoutSecs := int(bleDialTimeout / time.Second)

	conn, err := central.Connect(address, timeoutSecs)
	if err != nil {
		return err
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

	if err := conn.OpenL2CAP(psm, timeoutSecs); err != nil {
		conn.Close()
		return fmt.Errorf("opening L2CAP channel (PSM %d): %w", psm, err)
	}

	if tlsCfg == nil {
		tlsCfg = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec — unprovisioned devices serve a self-signed cert
			MinVersion:         tls.VersionTLS12,
		}
	}

	stream := central.NewL2CAPStream(conn)
	// Bound the handshake explicitly. A BLE read with no deadline blocks
	// indefinitely by design, because that is what the read loop below needs,
	// so a device that opens the channel and then says nothing would hang here
	// forever.
	_ = stream.SetDeadline(time.Now().Add(bleDialTimeout))
	tlsConn := tls.Client(stream, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		// Through the stream, not the connection: the stream is what tracks
		// whether the underlying connection has already been closed.
		stream.Close() //nolint:errcheck,gosec — teardown on an already-failed path
		return fmt.Errorf("BLE TLS handshake: %w", err)
	}
	// Clear it before the read loop can inherit an expired deadline.
	_ = stream.SetDeadline(time.Time{})

	c.link = newDirectLink(tlsConn)
	if err := c.handshake(); err != nil {
		tlsConn.Close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.startReadLoop()
	return nil
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
			return nil
		},
	}
	return c.ConnectViaBLE(address, psm, tlsCfg)
}
