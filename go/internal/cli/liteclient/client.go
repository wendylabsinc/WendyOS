// Package liteclient adapts CLI-owned connections and files to WendyGo Lite.
package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/lite"
	sdkpb "github.com/wendylabsinc/wendy-go/lite/pb"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/seriallock"
	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.bug.st/serial"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// ErrSerialPortUnavailable identifies a failure before the serial port opened.
var ErrSerialPortUnavailable = errors.New("serial port unavailable")

type DeviceIdentity = lite.DeviceIdentity
type DeviceInfo = lite.DeviceInfo
type AppType = lite.AppType
type ConfPushMode = lite.ConfPushMode
type ConsoleChunk = lite.ConsoleChunk

const (
	AppTypeWasm         = lite.AppTypeWasm
	AppTypeNative       = lite.AppTypeNative
	ConfPushModeReplace = lite.ConfPushModeReplace
	ConfPushModeUpdate  = lite.ConfPushModeUpdate
	versionMajor        = 2
)

// WendyLiteClient retains only host connection state. WendyGo owns WendyCom.
type WendyLiteClient struct {
	*lite.Client
	link       wcomLink
	peerCert   *x509.Certificate
	serialLock *seriallock.Lock
	closeOnce  sync.Once
	closeErr   error
}

func NewWendyLiteClient() *WendyLiteClient {
	return &WendyLiteClient{Client: lite.NewClient(wendy.CryptoEntropy{})}
}

// PeerCertificate returns the device's leaf certificate, or nil when the
// connection did not verify it. Authenticated LAN, BLE, and Cloud relay
// connections set it; serial and unverified connections return nil.
func (c *WendyLiteClient) PeerCertificate() *x509.Certificate {
	return c.peerCert
}

func (c *WendyLiteClient) ConnectInsecure(address string) error {
	return c.ConnectInsecureContext(context.Background(), address)
}

// ConnectInsecureContext bounds both TLS and WendyCom handshakes.
func (c *WendyLiteClient) ConnectInsecureContext(ctx context.Context, address string) error {
	dialer := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec -- unprovisioned devices use self-signed certificates
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	c.link = newDirectLink(conn)
	err = c.handshake()
	if err != nil {
		c.link.close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

func (c *WendyLiteClient) ConnectWithMutualAuthentication(address string, cert tls.Certificate, rootCAs x509.CertPool) error {
	return c.ConnectWithMutualAuthenticationContext(context.Background(), address, cert, rootCAs)
}

// ConnectWithMutualAuthenticationContext bounds the TCP, TLS, and WendyCom
// handshakes by ctx.
func (c *WendyLiteClient) ConnectWithMutualAuthenticationContext(ctx context.Context, address string, cert tls.Certificate, rootCAs x509.CertPool) error {
	// Verify the certificate chain against our root CAs but skip hostname
	// checking — devices on a local network don't have SANs.
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, len(rawCerts))
			for i, raw := range rawCerts {
				c, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parsing server certificate: %w", err)
				}
				certs[i] = c
			}
			opts := x509.VerifyOptions{
				Roots:         &rootCAs,
				Intermediates: x509.NewCertPool(),
			}
			for _, c := range certs[1:] {
				opts.Intermediates.AddCert(c)
			}
			if _, err := certs[0].Verify(opts); err != nil {
				return fmt.Errorf("server certificate verification failed: %w", err)
			}
			return nil
		},
	}
	return c.connectTLSContext(ctx, address, tlsCfg)
}

func (c *WendyLiteClient) ConnectToSerial(device string) error {
	return c.ConnectToSerialContext(context.Background(), device)
}

// ConnectToSerialContext allows cancellation of the serial WendyCom handshake.
func (c *WendyLiteClient) ConnectToSerialContext(ctx context.Context, device string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := seriallock.Acquire(device)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSerialPortUnavailable, err)
	}
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(device, mode)
	if err != nil {
		lock.Release()
		return fmt.Errorf("%w: open serial: %w", ErrSerialPortUnavailable, err)
	}
	link := newSerialLink(port)
	c.link = link
	c.serialLock = lock
	if err := c.handshakeSerialContext(ctx, link); err != nil {
		link.close() // also stops the keep-alive loop linkHandshake may have started
		lock.Release()
		c.link = nil
		c.serialLock = nil
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

func (c *WendyLiteClient) handshakeSerialContext(ctx context.Context, link *directLink) error {
	stopCancel := context.AfterFunc(ctx, func() { link.close() })
	err := c.handshakeContext(ctx)
	if !stopCancel() {
		// close is idempotent and joins the cancellation callback's teardown.
		// Never return a live client while cancellation still closes its port.
		link.close()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ConnectViaRelay authenticates directly to the device over an opaque broker
// stream. The supplied Cloud connection and metadata retain their normal auth.
// cert carries the operator's presentation chain; deviceTrustChain verifies
// the device, whose issuing authority can differ from the operator's.
// This method owns cc on every path, including invalid local credentials.
func (c *WendyLiteClient) ConnectViaRelay(ctx context.Context, cc *grpc.ClientConn, assetID string,
	cert tls.Certificate, deviceTrustChain, deviceID string, options RelayOptions) error {
	cfg, err := deviceTLSConfig(ctx, cert, deviceTrustChain, deviceID, options.DeviceTrustFallback)
	if err != nil {
		cc.Close()
		return err
	}
	raw, err := openRelayConn(ctx, cc, assetID, options)
	if err != nil {
		return err
	}
	// The opaque relay can cross a BLE mesh uplink. Bound setup without
	// assuming a direct Internet path; the caller can still cancel sooner.
	handshakeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		raw.Close()
		return fmt.Errorf("device mTLS over relay: %w", err)
	}
	deadline, _ := handshakeCtx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		raw.Close()
		return err
	}
	if _, err := conn.Write(raw.grant); err != nil {
		raw.Close()
		return fmt.Errorf("forward Lite grant: %w", err)
	}
	var ack [4]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		raw.Close()
		return fmt.Errorf("device refused Lite session grant: %w", err)
	}
	if string(ack[:]) != "WLA2" {
		raw.Close()
		return fmt.Errorf("device refused Lite session grant")
	}
	if err := raw.established(); err != nil {
		raw.Close()
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		raw.Close()
		return err
	}
	// Close the raw pipe first so TLS shutdown cannot block behind a stalled
	// broker write. No application message is sent until mTLS succeeds.
	c.link = newDirectLink(&relayTLSConn{Conn: conn, raw: raw})
	if err := c.handshake(); err != nil {
		c.link.close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.peerCert = conn.ConnectionState().PeerCertificates[0]
	return nil
}

type relayTLSConn struct {
	*tls.Conn
	raw *relayConn
}

func (c *relayTLSConn) Close() error { return c.raw.Close() }

func (c *WendyLiteClient) Close() error {
	if c.link == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.link.close()
		err := c.Client.Close()
		if c.closeErr == nil {
			c.closeErr = err
		}
		c.serialLock.Release()
	})
	return c.closeErr
}

// handshake keeps the host's existing diagnostic prefixes around Core errors.
func (c *WendyLiteClient) handshake() error { return c.handshakeContext(context.Background()) }
func (c *WendyLiteClient) handshakeContext(ctx context.Context) error {
	err := c.Client.Connect(ctx, linkAdapter{c.link})
	if err != nil && errors.Unwrap(err) != nil && strings.HasPrefix(err.Error(), "handshake: ") {
		return errors.Unwrap(err)
	}
	return err
}

type linkAdapter struct{ wcomLink }

func (l linkAdapter) Handshake() error { return l.linkHandshake() }
func (l linkAdapter) Send(data []byte) error {
	m := new(wendypb.WendyComMessage)
	if err := proto.Unmarshal(data, m); err != nil {
		return err
	}
	return l.send(m)
}
func (l linkAdapter) Receive(timeout time.Duration) ([]byte, error) {
	m, err := l.recv(timeout)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(m)
}
func (l linkAdapter) PreferredChunkSize() int { return l.preferredChunkSize() }
func (l linkAdapter) Close() error            { return l.close() }
func copyMessage(src, dst proto.Message) error {
	data, err := proto.Marshal(src)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, dst)
}
func (c *WendyLiteClient) PushApp(path string, kind AppType, progress func(uint32, uint32)) error {
	if kind != AppTypeWasm && kind != AppTypeNative {
		return fmt.Errorf("unknown app type %d", kind)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	return c.Client.PushApp(f, info.Size(), kind, progress)
}
func (c *WendyLiteClient) PushConf(conf *wendypb.WendyConf, mode ConfPushMode, progress func(uint32, uint32)) error {
	if mode != ConfPushModeReplace && mode != ConfPushModeUpdate {
		return fmt.Errorf("unknown conf push mode %d", mode)
	}
	converted := new(sdkpb.WendyConf)
	if err := copyMessage(conf, converted); err != nil {
		return err
	}
	return c.Client.PushConf(converted, mode, progress)
}
func (c *WendyLiteClient) GetSensorManifest(timeout time.Duration) (*sensorlinkpb.SensorManifest, error) {
	m, err := c.Client.GetSensorManifest(timeout)
	if err != nil {
		return nil, err
	}
	out := new(sensorlinkpb.SensorManifest)
	err = copyMessage(m, out)
	return out, err
}
func (c *WendyLiteClient) EnrollmentChallenge(statusOnly bool) (*wendypb.WendyComEnrollmentChallenge, error) {
	m, err := c.Client.EnrollmentChallenge(statusOnly)
	if err != nil {
		return nil, err
	}
	out := new(wendypb.WendyComEnrollmentChallenge)
	err = copyMessage(m, out)
	return out, err
}
func (c *WendyLiteClient) AddSensorDataListener(fn func(*sensorlinkpb.SensorData)) func() {
	return c.Client.AddSensorDataListener(func(m *sdkpb.SensorData) {
		// SensorData contains numeric fields and bytes only; this wire projection
		// cannot fail UTF-8 validation and preserves future unknown fields.
		out := new(sensorlinkpb.SensorData)
		_ = copyMessage(m, out)
		fn(out)
	})
}

// deviceTLSConfig verifies the device against chain and presents cert unchanged.
// Callers supply the operator's own intermediates in cert; the device trust
// bundle may belong to a different authority and must not be sent as that chain.
// It is the one device verifier for relay, LAN and BLE. A non-nil lite starts
// here, before dialing, in its own goroutine under ctx; only a board issued by
// the Lite Device CA waits for it, so a board under the session CA never waits
// on PKI reachability.
func deviceTLSConfig(ctx context.Context, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) (*tls.Config, error) {
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("missing client certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	opts := certs.ServerVerifyOpts{ChainPEM: chain}
	identity, hasIdentity, err := certs.IdentityFromCert(leaf)
	if err != nil {
		return nil, err
	}
	if !hasIdentity || identity.TenantUUID == "" || !strings.HasPrefix(identity.Principal, "spiffe://wendy.sh/tenant/"+identity.TenantUUID+"/operator/") {
		return nil, fmt.Errorf("Wendy Lite PKI requires a tenant-scoped operator identity")
	}
	if deviceID == "" {
		return nil, fmt.Errorf("missing Wendy Lite device identity")
	}
	// A Lite board's one id is a canonical device UUID. Any other pin, such as
	// a pre-UUID lite- identity, is refused rather than used verbatim.
	if id, err := uuid.Parse(deviceID); err != nil || id.String() != deviceID {
		return nil, fmt.Errorf("Wendy Lite device identity %q is not a device UUID; update the board's firmware, then unenroll and re-enroll it over USB", deviceID)
	}
	opts.ExpectedIdentity = &certs.WendyIdentity{
		EntityType: certs.EntityAsset, EntityID: deviceID, TenantUUID: identity.TenantUUID,
		Principal: "spiffe://wendy.sh/tenant/" + identity.TenantUUID + "/device/" + deviceID,
	}

	verify, err := certs.BuildServerVerifyConnection(opts)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		InsecureSkipVerify: true, // Verification and device identity checks are performed below.
		VerifyConnection:   verify,
	}
	if lite != nil {
		type loaded struct {
			chain string
			err   error
		}
		result := make(chan loaded, 1)
		go func() {
			chainPEM, err := lite(ctx)
			result <- loaded{chainPEM, err}
		}()
		addLiteDeviceTrust(cfg, cert, chain, deviceID, sync.OnceValues(func() (string, error) {
			r := <-result
			return r.chain, r.err
		}))
	}
	return cfg, nil
}

func (c *WendyLiteClient) ConnectWithPKIAuthentication(address string, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) error {
	return c.ConnectWithPKIAuthenticationContext(context.Background(), address, cert, chain, deviceID, lite)
}

// ConnectWithPKIAuthenticationContext bounds connection establishment and both
// the TLS and WendyCom handshakes by ctx. lite may be nil (see deviceTLSConfig).
func (c *WendyLiteClient) ConnectWithPKIAuthenticationContext(ctx context.Context, address string, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) error {
	cfg, err := deviceTLSConfig(ctx, cert, chain, deviceID, lite)
	if err != nil {
		return err
	}
	return c.connectTLSContext(ctx, address, cfg)
}

// connectTLSContext uses an already configured verifier for the TLS handshake,
// then establishes WendyCom under the same cancellation context.
func (c *WendyLiteClient) connectTLSContext(ctx context.Context, address string, cfg *tls.Config) error {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: cfg}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect (mTLS): %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	c.link = newDirectLink(conn)
	if err := c.handshake(); err != nil {
		c.link.close()
		c.link = nil
		return fmt.Errorf("handshake: %w", err)
	}
	c.peerCert = conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
	return nil
}

// ConnectSensorSource authenticates a pinned SensorLink source using this
// consumer's device identity. The source limits device callers to SensorLink.
func (c *WendyLiteClient) ConnectSensorSource(ctx context.Context, address string, cert tls.Certificate, chain, principal string) error {
	if len(cert.Certificate) == 0 {
		return fmt.Errorf("missing consumer certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	own, ok, err := certs.IdentityFromCert(leaf)
	if err != nil || !ok || own.EntityType != certs.EntityAsset || own.TenantUUID == "" {
		return fmt.Errorf("SensorLink requires a tenant-scoped consumer device identity")
	}
	expected, err := certs.ParseDevicePrincipal(principal)
	if err != nil || principal != certs.DeviceSPIFFEURI(expected.TenantUUID, expected.EntityID) || !expected.Scope().Matches(own.Scope()) {
		return fmt.Errorf("SensorLink source must be a device in the consumer's tenant")
	}
	verify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{ChainPEM: chain, ExpectedIdentity: &expected})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		InsecureSkipVerify: true, // VerifyConnection authenticates the full chain and exact device principal.
		VerifyConnection:   verify}
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect SensorLink (mTLS): %w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c.link = newDirectLink(conn)
	if err := c.handshake(); err != nil {
		c.link.close()
		c.link = nil
		return fmt.Errorf("SensorLink handshake: %w", err)
	}
	c.peerCert = conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
	return nil
}
