//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const tcpConfigName = "local-mesh.json"

// RunConfiguredTCP runs the explicitly configured topology. The configured
// peer list controls eligibility; crossed authenticated connections converge
// to one preferred direction. A missing config leaves the local mesh disabled.
func RunConfiguredTCP(parent context.Context, configDir string, id TCPIdentity) error {
	return RunConfiguredTCPObserved(parent, configDir, id, nil)
}

// RunConfiguredTCPObserved publishes the running node to an observer. The
// observer receives nil when the runtime exits; consumers also check the
// current route snapshot before using it.
func RunConfiguredTCPObserved(parent context.Context, configDir string, id TCPIdentity, observe func(func() NodeSnapshot)) error {
	return RunConfiguredWithNode(parent, configDir, id, func(node *Node, _ *TCPConfig) {
		if observe == nil {
			return
		}
		if node == nil {
			observe(nil)
		} else {
			observe(node.Snapshot)
		}
	})
}

// RunConfiguredWithNode owns one Babel node shared by configured TCP and
// optional radio providers. The observer may start providers and must stop
// them when it receives nil before the node is closed.
func RunConfiguredWithNode(parent context.Context, configDir string, id TCPIdentity, observe func(*Node, *TCPConfig)) error {
	return runConfiguredWithNode(parent, configDir, id, observe, net.Listen)
}

func runConfiguredWithNode(parent context.Context, configDir string, id TCPIdentity, observe func(*Node, *TCPConfig), listen func(string, string) (net.Listener, error)) error {
	credentials, err := NewCredentials(id.Certificate, id.Chain, id.Key)
	if err != nil {
		return err
	}
	cfg, err := LoadTCPConfig(filepath.Join(configDir, tcpConfigName), credentials.Device)
	if err != nil || cfg == nil {
		return err
	}
	stateDir := filepath.Join(configDir, "local-mesh")
	if err = os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	// Keep the node alive while radio providers drain their links on parent
	// shutdown. The runtime cancels it only after the observer has stopped them.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	var listener net.Listener
	if cfg.Listen != "" {
		listener, err = listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		defer listener.Close()
	}
	var tlsListener net.Listener
	if cfg.TLSListen != "" {
		tlsListener, err = listen("tcp", cfg.TLSListen)
		if err != nil {
			return fmt.Errorf("listen for native TLS peers: %w", err)
		}
		defer tlsListener.Close()
	}
	node, err := NewNode(ctx, stateDir, credentials, id.Name, id.AgentPort)
	if err != nil {
		return err
	}
	if listener != nil {
		go func() { <-ctx.Done(); listener.Close() }()
	}
	nodeDone := make(chan error, 1)
	go func() { nodeDone <- node.Run(); cancel() }()
	policy := NewConnectionPolicy(credentials.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})
	var tlsDone chan error
	if tlsListener != nil {
		tlsDone = make(chan error, 1)
		go func() { tlsDone <- runNetTLSListener(ctx, node, tlsListener, WiFiLinkCost, policy, "") }()
	}
	if observe != nil {
		observe(node, cfg)
	}
	allowed := make(map[string]meshid.Device, len(cfg.Peers))
	for _, peer := range cfg.devices {
		allowed[peer.Principal()] = peer
	}
	queue := &CandidateQueue{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		keys := make([]string, 0, len(allowed))
		for key := range allowed {
			keys = append(keys, key)
		}
		for ctx.Err() == nil {
			for _, key := range queue.Order(keys, time.Now()) {
				peer := allowed[key]
				lease := policy.Reserve(key, peer, true)
				if lease == nil {
					continue
				}
				lease.ObserveReady(func() { queue.Succeeded(key) })
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer lease.Release()
					defer func() {
						if ctx.Err() == nil && !lease.Retired() {
							queue.Failed(key, time.Now())
						}
					}()
					address := cfg.Peers[indexOfDevice(cfg.devices, peer)].Address
					dialer := net.Dialer{Timeout: 5 * time.Second}
					conn, err := dialer.DialContext(ctx, "tcp", address)
					if err != nil {
						return
					}
					defer conn.Close()
					if writeTCPPreface(conn, credentials.Device) == nil {
						_ = attachTCP(WithConnectionLease(ctx, lease), node, credentials, peer, conn, false)
					}
				}()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	sem := make(chan struct{}, LANMaxConnections)
	acceptDone := make(chan struct{})
	if listener == nil {
		close(acceptDone)
	} else {
		go func() {
			defer close(acceptDone)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				select {
				case sem <- struct{}{}:
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer func() { <-sem }()
						defer conn.Close()
						lease := policy.Reserve(fmt.Sprintf("incoming:%p", conn), meshid.Device{}, false)
						if lease == nil {
							return
						}
						defer lease.Release()
						conn = lease.TrackIncoming(conn)
						id, err := readTCPPreface(conn, credentials.Device.Tenant())
						if err != nil {
							return
						}
						peer, err := meshid.ParseDevice(credentials.Device.Tenant(), id.String())
						if err != nil || peer == credentials.Device {
							return
						}
						if _, ok := allowed[peer.Principal()]; !ok {
							return
						}
						lease.ObserveReady(func() { queue.Succeeded(peer.Principal()) })
						defer func() {
							if lease.WasReady() && !lease.Retired() && ctx.Err() == nil {
								queue.Failed(peer.Principal(), time.Now())
							}
						}()
						_ = attachTCP(WithConnectionLease(ctx, lease), node, credentials, peer, conn, true)
					}()
				default:
					conn.Close()
				}
			}
		}()
	}
	nodeStopped, tlsStopped := false, false
	select {
	case <-parent.Done():
	case err = <-nodeDone:
		nodeStopped = true
		if errors.Is(err, context.Canceled) {
			err = nil
		}
	case err = <-tlsDone:
		tlsStopped = true
	}
	if observe != nil {
		observe(nil, nil)
	}
	cancel()
	if listener != nil {
		listener.Close()
	}
	<-acceptDone
	wg.Wait()
	if tlsDone != nil && !tlsStopped {
		err = errors.Join(err, <-tlsDone)
	}
	if !nodeStopped {
		// Normal node cancellation must not hide an earlier listener failure
		// when the independent runtime results are combined.
		if nodeErr := <-nodeDone; !errors.Is(nodeErr, context.Canceled) {
			err = errors.Join(err, nodeErr)
		}
	}
	return err
}

func indexOfDevice(devices []meshid.Device, want meshid.Device) int {
	for i, d := range devices {
		if d.Principal() == want.Principal() {
			return i
		}
	}
	return -1
}

// writeTCPPreface announces this device's tenant and device UUID before the
// QUIC handshake, so a listener can order the edge and reject a peer it has no
// configured relationship with. The preface is not trusted identity: the TLS
// handshake pins the full principal afterwards.
func writeTCPPreface(conn net.Conn, device meshid.Device) error {
	if !device.IsValid() {
		return errors.New("TCP preface requires a device identity")
	}
	tenant, id := device.Tenant().UUID(), device.UUID()
	data := make([]byte, 0, 4+16+16)
	data = append(data, "WMT1"...)
	data = append(data, tenant[:]...)
	data = append(data, id[:]...)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	err := writeFull(conn, data)
	conn.SetDeadline(time.Time{})
	return err
}

func readTCPPreface(conn net.Conn, tenant meshid.Tenant) (uuid.UUID, error) {
	if !tenant.IsValid() {
		return uuid.Nil, errors.New("TCP preface requires a tenant identity")
	}
	data := make([]byte, 4+16+16)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadFull(conn, data)
	conn.SetDeadline(time.Time{})
	if err != nil {
		return uuid.Nil, err
	}
	want := tenant.UUID()
	if string(data[:4]) != "WMT1" || !bytes.Equal(data[4:20], want[:]) {
		return uuid.Nil, errors.New("invalid wendy net TCP preface")
	}
	var id uuid.UUID
	copy(id[:], data[20:])
	return id, nil
}

func attachTCP(ctx context.Context, node *Node, credentials *Credentials, peer meshid.Device, tcp net.Conn, server bool) error {
	return attachTCPWithCost(ctx, node, credentials, peer, tcp, server, 256)
}
func attachTCPWithCost(ctx context.Context, node *Node, credentials *Credentials, peer meshid.Device, tcp net.Conn, server bool, cost uint16) error {
	// The caller selected an eligible peer from its preface or discovery.
	// Associate that scheduling hint before QUIC I/O so an inbound handshake
	// can suppress a crossed outgoing attempt. TLS still pins the full peer.
	lease, _ := ctx.Value(connectionLeaseKey{}).(*ConnectionLease)
	lease.Identify(peer)
	pc, err := NewTCPPacketConn(tcp)
	if err != nil {
		return err
	}
	transport := &quic.Transport{Conn: pc}
	defer transport.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			transport.Close()
		case <-done:
		}
	}()
	tlsConfig, err := credentials.PeerTLSWithTickets(peer, LinkALPN, LinkQUICSessionScope)
	if err != nil {
		return err
	}
	var conn *quic.Conn
	handshake, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if server {
		listener, err := transport.Listen(tlsConfig, QUICConfig())
		if err != nil {
			return err
		}
		defer listener.Close()
		conn, err = listener.Accept(handshake)
		if err != nil {
			return fmt.Errorf("wendy net QUIC peer %s: %w", peer.Principal(), err)
		}
	} else {
		conn, err = transport.Dial(handshake, pc.RemoteAddr(), tlsConfig, QUICConfig())
		if err != nil {
			return fmt.Errorf("wendy net QUIC peer %s: %w", peer.Principal(), err)
		}
	}
	// quic-go can return a nil accepted connection after rejecting an
	// unauthenticated peer. Treat it as a failed handshake before Node sees it.
	if conn == nil {
		return fmt.Errorf("wendy net QUIC peer %s: no authenticated connection", peer.Principal())
	}
	return node.AttachWithCost(WithCarrier(ctx, carrierName(ctx, "tcp")), peer, conn, cost)
}
