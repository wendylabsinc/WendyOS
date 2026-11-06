package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc/credentials"
)

const deliveryHandshakeTimeout = 8 * time.Second

// A connected LAN socket may belong to a stale or spoofed advertisement.
// Retry only the transport: both attempts run the identical TLS configuration
// and full-principal pin before any RPC or image content can be sent.
func authenticateDelivery(ctx context.Context, peers PeerDialer, target DeliveryTarget, port uint16, raw net.Conn, mode string, handshake func(context.Context, net.Conn) (net.Conn, error)) (net.Conn, error) {
	authenticate := func(raw net.Conn) (net.Conn, error) {
		bounded, cancel := context.WithTimeout(ctx, deliveryHandshakeTimeout)
		defer cancel()
		conn, err := handshake(bounded, raw)
		if err != nil {
			raw.Close()
		}
		return conn, err
	}
	conn, firstErr := authenticate(raw)
	if firstErr == nil {
		return conn, nil
	}
	routed, ok := peers.(interface {
		DialRoutedTarget(context.Context, DeliveryTarget, uint16) (net.Conn, string, error)
	})
	if mode != "lan-direct" || !ok || ctx.Err() != nil {
		return nil, firstErr
	}
	raw, _, err := routed.DialRoutedTarget(ctx, target, port)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("LAN authentication: %w", firstErr), fmt.Errorf("Babel dial: %w", err))
	}
	conn, err = authenticate(raw)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("LAN authentication: %w", firstErr), fmt.Errorf("Babel authentication: %w", err))
	}
	return conn, nil
}

// TLS authentication completes in the dial callback so gRPC owns the selected
// socket before it starts monitoring connection cancellation and HTTP/2 setup.
// Its normal TLS credentials perform that handshake; this adapter returns their
// verified auth info without wrapping the already-secure connection a second time.
type deliveryAuthenticatedConn struct {
	net.Conn
	authInfo credentials.AuthInfo
}

type deliveryCredentials struct {
	credentials.TransportCredentials
}

func (c *deliveryCredentials) ClientHandshake(ctx context.Context, _ string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	authenticated, ok := raw.(*deliveryAuthenticatedConn)
	if !ok || authenticated.authInfo == nil {
		return nil, nil, errors.New("build delivery dialer did not authenticate TLS")
	}
	return authenticated, authenticated.authInfo, nil
}

func (c *deliveryCredentials) Clone() credentials.TransportCredentials {
	return &deliveryCredentials{TransportCredentials: c.TransportCredentials.Clone()}
}
