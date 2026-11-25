package commands

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// registryDialerFor is how the CLI reaches the device registry when it is not
// a plain dial of the device's address: the cloud tunnel, or, on a Closed
// device reached with this computer's key, TLS with that key. nil means dial
// the address directly over plain HTTP, as for an Open device.
func registryDialerFor(ctx context.Context, conn *grpcclient.AgentConnection) (func(context.Context, int) (net.Conn, error), error) {
	if !conn.IsLocalOperator {
		return conn.RegistryDialer, nil
	}
	access, err := agentpbv2.NewWendyAccessServiceClient(conn.Conn).GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil {
		return nil, fmt.Errorf("reading the device's access shape for its registry: %w", err)
	}
	if access.GetKeylessApps() {
		return nil, nil // keyless apps on: the registry is plain HTTP, as on an Open device
	}
	cfg, err := localRegistryTLSConfig(conn)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, port int) (net.Conn, error) {
		return tlsDialerWith(cfg, func(ctx context.Context) (net.Conn, error) {
			return deviceLinkDialFn(ctx, net.JoinHostPort(conn.Host, strconv.Itoa(port)))
		})(ctx)
	}, nil
}

// localRegistryTLSConfig presents this computer's key (the registry of a
// Closed device admits operator and owner keys) and accepts only the device
// key the gRPC connection verified: the device serves its registry under the
// same self-signed key.
func localRegistryTLSConfig(conn *grpcclient.AgentConnection) (*tls.Config, error) {
	deviceKey := conn.LocalDeviceKey()
	if deviceKey == nil {
		return nil, commandErrorf(errRegistryAuth, "the device's key was not verified on this connection; cannot authenticate its registry")
	}
	key, err := localOperatorKeyFn()
	if err != nil {
		return nil, commandErrorf(errRegistryAuth, "loading this computer's local key: %v", err)
	}
	cert, err := localkey.Certificate(key, time.Now())
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Authenticated by the device key in VerifyConnection, not a chain.
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("device registry presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if _, found, err := certs.IdentityFromCert(leaf); found || err != nil {
				return errors.New("device registry presented a cloud-enrolled certificate to a Closed-device connection")
			}
			if !bytes.Equal(leaf.RawSubjectPublicKeyInfo, deviceKey) {
				return errors.New("device registry presented a different key than the device")
			}
			return leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature)
		},
	}, nil
}

// tlsDialerWith wraps dial so every connection is upgraded to TLS with cfg
// before use.
func tlsDialerWith(cfg *tls.Config, dial func(context.Context) (net.Conn, error)) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		raw, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(raw, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("TLS handshake with device registry: %w", err)
		}
		return tlsConn, nil
	}
}
