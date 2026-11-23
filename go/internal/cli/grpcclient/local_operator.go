package grpcclient

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// ErrNotLocalDevice is the local-operator verifier's refusal of a device that
// presented a CA-issued (cloud-enrolled) certificate: such a device refuses
// local keys, so the operator's cloud identity is the credential for it.
var ErrNotLocalDevice = errors.New("device is cloud-enrolled; local operator keys are not used")

// ConnectWithLocalKey dials a device that is not cloud-enrolled with the
// operator's local key (WDY-3882): the client certificate is self-issued over
// key (localkey.Certificate), and the device must present a certificate
// self-signed over its own key that names no tenant. checkServer, when
// non-nil, decides whether that device key is the expected one (the SPKI pin);
// a *devicepin.PinMismatchError from it fails the handshake and is readable
// through PinMismatch, any other error from it is bookkeeping and ignored.
func ConnectWithLocalKey(ctx context.Context, address string, key crypto.Signer, checkServer func(*x509.Certificate) error) (*AgentConnection, error) {
	cert, err := localkey.Certificate(key, time.Now())
	if err != nil {
		return nil, err
	}
	notLocal, localDevice := new(atomic.Bool), new(atomic.Pointer[[]byte])
	pinMismatch := new(atomic.Pointer[devicepin.PinMismatchError])
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// The device's certificate is self-signed; it is authenticated by its
		// key in VerifyConnection, not by a chain or hostname.
		InsecureSkipVerify: true, //nolint:gosec
		// ML-DSA keys need TLS 1.3.
		MinVersion: tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("device presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if _, found, err := certs.IdentityFromCert(leaf); found || err != nil {
				notLocal.Store(true)
				return ErrNotLocalDevice
			}
			if leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
				return errors.New("device certificate is not self-signed by its key")
			}
			if checkServer != nil {
				var pm *devicepin.PinMismatchError
				if err := checkServer(leaf); errors.As(err, &pm) {
					pinMismatch.Store(pm)
					return pm
				}
			}
			spki := leaf.RawSubjectPublicKeyInfo
			localDevice.Store(&spki)
			return nil
		},
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithInitialWindowSize(grpcInitialStreamWindow),
		grpc.WithInitialConnWindowSize(grpcInitialConnWindow),
		grpc.WithReadBufferSize(grpcReadBufferSize),
		grpc.WithWriteBufferSize(grpcWriteBufferSize),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                grpcKeepaliveTime,
			Timeout:             grpcKeepaliveTimeout,
			PermitWithoutStream: false,
		}),
	}
	dialOpts, tcpDial := agentTCPDialOptions(address)
	conn, err := grpc.NewClient(grpcTarget(address), append(opts, dialOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("connecting to agent at %s with the local operator key: %w", address, err)
	}
	ac := newAgentConnection(conn)
	ac.Host = hostFromAddress(address)
	ac.Addr = address
	ac.IsMTLS = true
	ac.IsLocalOperator = true
	ac.pinMismatch = pinMismatch
	ac.notLocalDevice = notLocal
	ac.localDeviceVerified = localDevice
	ac.tcpDial = tcpDial
	return ac, nil
}

// NotLocalDevice reports whether the device refused the local-operator dial by
// presenting a cloud-enrolled certificate (see ErrNotLocalDevice).
func (c *AgentConnection) NotLocalDevice() bool {
	return c != nil && c.notLocalDevice != nil && c.notLocalDevice.Load()
}

// LocalDeviceVerified reports whether the device presented the certificate of
// a local-only device (self-signed, no tenant, matching any pin): a refusal
// after that is the device refusing the local key.
func (c *AgentConnection) LocalDeviceVerified() bool {
	return c.LocalDeviceKey() != nil
}

// LocalDeviceKey is the SubjectPublicKeyInfo of the local-only device this
// connection verified, nil before (or without) that. The device serves its
// registry under the same key, so registry connections are bound to it.
func (c *AgentConnection) LocalDeviceKey() []byte {
	if c == nil || c.localDeviceVerified == nil {
		return nil
	}
	if spki := c.localDeviceVerified.Load(); spki != nil {
		return *spki
	}
	return nil
}
