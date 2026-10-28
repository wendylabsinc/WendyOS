//go:build linux

package localmesh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func nativeServerTLS(c *Credentials) (*tls.Config, error) {
	if c == nil || c.Verify == nil || len(c.Certificate.Certificate) == 0 {
		return nil, errors.New("native listener requires enrolled credentials")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{c.Certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{StreamALPN}, SessionTicketsDisabled: true,
		VerifyConnection: func(s tls.ConnectionState) error {
			if s.NegotiatedProtocol != StreamALPN {
				return errors.New("native stream ALPN required")
			}
			_, err := lanAuthenticatedPeer(c, s)
			return err
		},
	}, nil
}

// RunTLSListener owns a native peer listener and drains every attachment before
// returning. All accepted peers use the ordinary authenticated stream protocol.
func RunTLSListener(ctx context.Context, node *Node, listener net.Listener, cost uint16) error {
	return runNetTLSListener(ctx, node, listener, cost, nil, "")
}

func runNetTLSListener(parent context.Context, node *Node, listener net.Listener, cost uint16, policy *ConnectionPolicy, group string) error {
	if listener == nil {
		return errors.New("native TLS listener missing")
	}
	defer listener.Close()
	if node == nil {
		return errors.New("native TLS node missing")
	}
	config, err := nativeServerTLS(node.Credentials)
	if err != nil {
		return err
	}
	if policy == nil {
		policy = NewConnectionPolicy(node.Credentials.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})
	}
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() { cancel(); _ = listener.Close(); workers.Wait() }()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	connections, handshakes := make(chan struct{}, LANMaxConnections), make(chan struct{}, LANMaxOutgoingAttempts)
	for {
		raw, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("native TLS listener: %w", err)
		}
		select {
		case connections <- struct{}{}:
		default:
			_ = raw.Close()
			continue
		}
		select {
		case handshakes <- struct{}{}:
		default:
			<-connections
			_ = raw.Close()
			continue
		}
		lease := policy.ReserveGroup(fmt.Sprintf("tls-incoming:%p", raw), meshid.Device{}, false, group)
		if lease == nil {
			<-handshakes
			<-connections
			_ = raw.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-connections }()
			defer lease.Release()
			defer raw.Close()
			stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
			defer stop()
			secure := tls.Server(lease.TrackIncoming(raw), config)
			setup, done := context.WithTimeout(ctx, 8*time.Second)
			err := secure.HandshakeContext(setup)
			done()
			<-handshakes
			if err != nil {
				return
			}
			peer, err := lanAuthenticatedPeer(node.Credentials, secure.ConnectionState())
			if err != nil {
				return
			}
			lease.Identify(peer)
			_ = node.AttachStream(WithConnectionLease(ctx, lease), peer, secure, cost)
		}()
	}
}
