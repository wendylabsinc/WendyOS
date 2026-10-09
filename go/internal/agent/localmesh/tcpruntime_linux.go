//go:build linux

package localmesh

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
)

const tcpConfigName = "local-mesh.json"

// RunConfiguredTCP runs the explicitly configured topology. The smaller asset
// initiates each edge, so a symmetric peer list creates one connection. A
// missing config leaves the local mesh disabled.
func RunConfiguredTCP(parent context.Context, configDir string, id TCPIdentity) error {
	return RunConfiguredTCPObserved(parent, configDir, id, nil)
}

// RunConfiguredTCPObserved publishes the running node to an observer. The
// observer receives nil when the runtime exits; consumers also check the
// current route snapshot before using it.
func RunConfiguredTCPObserved(parent context.Context, configDir string, id TCPIdentity, observe func(func() NodeSnapshot)) error {
	cfg, err := LoadTCPConfig(filepath.Join(configDir, tcpConfigName), id.Asset)
	if err != nil || cfg == nil {
		return err
	}
	credentials, err := NewCredentials(id.Org, id.Asset, id.Certificate, id.Chain, id.Key)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(configDir, "local-mesh")
	if err = os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	node, err := NewNode(ctx, stateDir, credentials, id.Name, id.AgentPort)
	if err != nil {
		return err
	}
	if observe != nil {
		observe(node.Snapshot)
		defer observe(nil)
	}
	go func() { <-ctx.Done(); listener.Close() }()
	nodeDone := make(chan error, 1)
	go func() { nodeDone <- node.Run(); cancel() }()
	allowed := make(map[int32]TCPPeer, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		allowed[peer.Asset] = peer
	}
	active := make(map[int32]bool)
	var activeMu sync.Mutex
	claim := func(asset int32) bool {
		activeMu.Lock()
		defer activeMu.Unlock()
		if active[asset] {
			return false
		}
		active[asset] = true
		return true
	}
	release := func(asset int32) { activeMu.Lock(); delete(active, asset); activeMu.Unlock() }
	var wg sync.WaitGroup
	for _, peer := range cfg.Peers {
		if id.Asset >= peer.Asset {
			continue
		}
		wg.Add(1)
		go func(peer TCPPeer) {
			defer wg.Done()
			backoff := time.Second
			for ctx.Err() == nil {
				if claim(peer.Asset) {
					conn, err := net.DialTimeout("tcp", peer.Address, 5*time.Second)
					if err == nil {
						err = writeTCPPreface(conn, id.Org, id.Asset)
						if err == nil {
							joined := time.Now()
							err = attachTCP(ctx, node, credentials, peer.Asset, conn, false)
							if time.Since(joined) >= 10*time.Second {
								// A stable session should reconnect promptly after a
								// later link loss. Keep exponential delay for flapping.
								backoff = time.Second
							}
						}
						conn.Close()
					}
					release(peer.Asset)
				}
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff = min(backoff*2, 30*time.Second)
			}
		}(peer)
	}
	sem := make(chan struct{}, 64)
	acceptDone := make(chan struct{})
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
					asset, err := readTCPPreface(conn, id.Org)
					if err != nil || asset >= id.Asset {
						return
					}
					if _, ok := allowed[asset]; !ok || !claim(asset) {
						return
					}
					defer release(asset)
					_ = attachTCP(ctx, node, credentials, asset, conn, true)
				}()
			default:
				conn.Close()
			}
		}
	}()
	nodeStopped := false
	select {
	case <-ctx.Done():
	case err = <-nodeDone:
		nodeStopped = true
	}
	cancel()
	listener.Close()
	<-acceptDone
	wg.Wait()
	if !nodeStopped {
		err = <-nodeDone
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func writeTCPPreface(conn net.Conn, org, asset int32) error {
	var data [12]byte
	copy(data[:4], "WMT1")
	binary.BigEndian.PutUint32(data[4:8], uint32(org))
	binary.BigEndian.PutUint32(data[8:12], uint32(asset))
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	err := writeFull(conn, data[:])
	conn.SetDeadline(time.Time{})
	return err
}

func readTCPPreface(conn net.Conn, org int32) (int32, error) {
	var data [12]byte
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadFull(conn, data[:])
	conn.SetDeadline(time.Time{})
	if err != nil {
		return 0, err
	}
	if string(data[:4]) != "WMT1" || int32(binary.BigEndian.Uint32(data[4:8])) != org {
		return 0, errors.New("invalid mesh TCP preface")
	}
	return int32(binary.BigEndian.Uint32(data[8:12])), nil
}

func attachTCP(ctx context.Context, node *Node, credentials *Credentials, peer int32, tcp net.Conn, server bool) error {
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
	tlsConfig, err := credentials.PeerTLSWithTickets(peer, LinkALPN, "configured-quic")
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
	} else {
		conn, err = transport.Dial(handshake, pc.RemoteAddr(), tlsConfig, QUICConfig())
	}
	if err != nil {
		return fmt.Errorf("mesh QUIC peer %d: %w", peer, err)
	}
	return node.Attach(ctx, peer, conn)
}
