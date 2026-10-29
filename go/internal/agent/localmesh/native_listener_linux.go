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
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

func nativeServerTLS(c *Credentials) (*tls.Config, error) {
	if c == nil || c.Verify == nil || len(c.Certificate.Certificate) == 0 {
		return nil, errors.New("native listener requires enrolled credentials")
	}
	protocols := []string{StreamALPN}
	if c.SupportsPrimaryOperators() {
		protocols = append(protocols, meshoperatorlink.RouterALPN)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{c.Certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert, NextProtos: protocols, SessionTicketsDisabled: true,
		VerifyConnection: func(s tls.ConnectionState) error {
			if ApplicationProtocol(s.NegotiatedProtocol) == meshoperatorlink.RouterALPN {
				chain := make([][]byte, len(s.PeerCertificates))
				for i, c := range s.PeerCertificates {
					chain[i] = c.Raw
				}
				_, err := c.VerifyPrimaryOperator(chain, time.Now())
				return err
			}
			if ApplicationProtocol(s.NegotiatedProtocol) != StreamALPN {
				return errors.New("native stream ALPN required")
			}
			_, err := lanAuthenticatedPeer(c, s)
			return err
		},
	}
	c.ConfigureTLSServer(config)
	return config, nil
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
	connections, handshakes := make(chan struct{}, LANMaxConnections+maxOperatorAttachments), make(chan struct{}, LANMaxOutgoingAttempts)
	deviceSlots := make(chan struct{}, LANMaxConnections)
	operatorSlots := make(chan struct{}, maxOperatorAttachments)
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
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-connections }()
			var lease *ConnectionLease
			var slot chan struct{}
			defer func() {
				if lease != nil {
					lease.Release()
				}
				if slot != nil {
					<-slot
				}
			}()
			defer raw.Close()
			stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
			defer stop()
			handshakeConfig := config.Clone()
			handshakeConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				selected := ""
				for _, supported := range config.NextProtos {
					for _, offered := range hello.SupportedProtos {
						if supported == offered {
							selected = supported
							break
						}
					}
					if selected != "" {
						break
					}
				}
				if slot == nil {
					if selected == meshoperatorlink.RouterALPN {
						slot = operatorSlots
					} else if selected == StreamALPN {
						slot = deviceSlots
					} else {
						return nil, errors.New("native stream ALPN required")
					}
					select {
					case slot <- struct{}{}:
					default:
						slot = nil
						return nil, errors.New("native carrier role capacity reached")
					}
					if selected == StreamALPN {
						lease = policy.ReserveGroup(fmt.Sprintf("tls-incoming:%p", raw), meshid.Device{}, false, group)
						if lease == nil {
							return nil, errors.New("device neighbour capacity reached")
						}
					}
				}
				return config.GetConfigForClient(hello)
			}
			secure := tls.Server(raw, handshakeConfig)
			setup, done := context.WithTimeout(ctx, 8*time.Second)
			err := secure.HandshakeContext(setup)
			done()
			<-handshakes
			if err != nil {
				return
			}
			if ApplicationProtocol(secure.ConnectionState().NegotiatedProtocol) == meshoperatorlink.RouterALPN {
				_ = node.AttachOperatorStream(WithCarrier(ctx, "tls-stream"), secure, cost)
				return
			}
			peer, err := lanAuthenticatedPeer(node.Credentials, secure.ConnectionState())
			if err != nil {
				return
			}
			lease.Identify(peer)
			lease.Progress()
			_ = node.AttachStream(WithConnectionLease(ctx, lease), peer, secure, cost)
		}()
	}
}
