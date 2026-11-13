//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func inCarrierNamespace(name string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return err
	}
	defer original.Close()
	target, err := netns.GetFromName(name)
	if err != nil {
		return err
	}
	defer target.Close()
	if err = netns.Set(target); err != nil {
		return err
	}
	defer func() {
		if err := netns.Set(original); err != nil {
			panic(err)
		}
	}()
	return fn()
}

type capturedCarrierFrame struct {
	at   time.Time
	data []byte
}

type carrierWireCapture struct {
	stop           chan struct{}
	done           chan struct{}
	fd             int
	mtu            int
	source, target net.IP
	mu             sync.Mutex
	packets        []capturedCarrierFrame
	fragmentCount  int
	largestPacket  int
	invalid        error
}

func startCarrierCapture(t *testing.T, ns, iface, source, target string, mtu int) *carrierWireCapture {
	t.Helper()
	c := &carrierWireCapture{stop: make(chan struct{}), done: make(chan struct{}), mtu: mtu, source: net.ParseIP(source), target: net.ParseIP(target)}
	err := inCarrierNamespace(ns, func() error {
		link, err := net.InterfaceByName(iface)
		if err != nil {
			return err
		}
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(uint16(unix.ETH_P_ALL<<8)))
		if err != nil {
			return err
		}
		c.fd = fd
		if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: uint16(unix.ETH_P_ALL << 8), Ifindex: link.Index}); err != nil {
			unix.Close(fd)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(c.done)
		buf := make([]byte, 4096)
		for {
			select {
			case <-c.stop:
				return
			default:
			}
			n, from, err := unix.Recvfrom(c.fd, buf, 0)
			if errors.Is(err, unix.EAGAIN) {
				_, _ = unix.Poll([]unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}}, 50)
				continue
			}
			if err != nil {
				c.mu.Lock()
				c.invalid = err
				c.mu.Unlock()
				return
			}
			addr, ok := from.(*unix.SockaddrLinklayer)
			if !ok || addr.Pkttype != unix.PACKET_OUTGOING || n < 14 {
				continue
			}
			frame := buf[:n]
			kind := binary.BigEndian.Uint16(frame[12:14])
			p := frame[14:]
			var source, target net.IP
			var fragmented bool
			var length int
			var udp bool
			var udpOffset int
			switch kind {
			case unix.ETH_P_IP:
				if len(p) < 20 {
					continue
				}
				source, target = net.IP(p[12:16]), net.IP(p[16:20])
				length = int(binary.BigEndian.Uint16(p[2:4]))
				fragmented = binary.BigEndian.Uint16(p[6:8])&0x3fff != 0
				udp = p[9] == 17
				udpOffset = int(p[0]&15) * 4
			case unix.ETH_P_IPV6:
				if len(p) < 40 {
					continue
				}
				source, target = net.IP(p[8:24]), net.IP(p[24:40])
				length = 40 + int(binary.BigEndian.Uint16(p[4:6]))
				fragmented = p[6] == 44
				udp = p[6] == 17
				udpOffset = 40
			default:
				continue
			}
			if !source.Equal(c.source) || !target.Equal(c.target) || (!udp && !fragmented) {
				continue
			}
			c.mu.Lock()
			if udp {
				if udpOffset < 20 || udpOffset+8 > len(p) {
					c.invalid = errors.New("truncated outgoing UDP header")
				} else {
					payload := int(binary.BigEndian.Uint16(p[udpOffset+4:udpOffset+6])) - 8
					c.largestPacket = max(c.largestPacket, payload)
					if payload < 0 || payload > carrierPacketLimit || payload+udpOffset+8 != length {
						c.invalid = fmt.Errorf("outgoing QUIC UDP payload %d exceeds ceiling %d or disagrees with IP length %d", payload, carrierPacketLimit, length)
					}
				}
			}
			if fragmented || length > c.mtu || length > len(p) {
				if fragmented {
					c.fragmentCount++
				}
				c.invalid = fmt.Errorf("outer IP fragmentation/size: fragmented=%t length=%d captured=%d mtu=%d", fragmented, length, len(p), c.mtu)
			}
			if len(c.packets) >= 4096 {
				c.invalid = errors.New("bounded carrier capture exceeded 4096 frames")
				c.mu.Unlock()
				return
			}
			c.packets = append(c.packets, capturedCarrierFrame{at: time.Now(), data: bytes.Clone(frame)})
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *carrierWireCapture) finish(t *testing.T, name string) {
	t.Helper()
	close(c.stop)
	<-c.done
	_ = unix.Close(c.fd)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid != nil {
		t.Error(c.invalid)
	}
	if len(c.packets) == 0 {
		t.Error("no actual outgoing QUIC packets captured")
	}
	dir := os.Getenv("WENDY_CARRIER_CAPTURE_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.CreateTemp(dir, name+"-*.pcap")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	path := f.Name()
	// Standard little-endian pcap, Ethernet link type. GSO is disabled before
	// the real UDP sockets are created; only sender-side frames are counted.
	for _, x := range []any{uint32(0xa1b2c3d4), uint16(2), uint16(4), uint32(0), uint32(0), uint32(65535), uint32(1)} {
		if err := binary.Write(f, binary.LittleEndian, x); err != nil {
			t.Fatal(err)
		}
	}
	sizes := make(map[int]int)
	for _, captured := range c.packets {
		p, now := captured.data, captured.at
		for _, x := range []uint32{uint32(now.Unix()), uint32(now.Nanosecond() / 1000), uint32(len(p)), uint32(len(p))} {
			if err := binary.Write(f, binary.LittleEndian, x); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.Write(p); err != nil {
			t.Fatal(err)
		}
		sizes[len(p)]++
	}
	result, _ := json.Marshal(map[string]any{"capture": path, "packets": len(c.packets), "ethernet_frame_sizes": sizes, "outer_ip_fragments": c.fragmentCount, "largest_quic_udp_payload": c.largestPacket, "interface_mtu": c.mtu, "sender_only": true, "gso_disabled": true})
	t.Log(string(result))
}

func carrierUDPAt(t *testing.T, ns, network, address string) *net.UDPConn {
	t.Helper()
	var conn *net.UDPConn
	if err := inCarrierNamespace(ns, func() error {
		addr, err := net.ResolveUDPAddr(network, address)
		if err != nil {
			return err
		}
		conn, err = net.ListenUDP(network, addr)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestCarrierWholeNativeUDPPaths(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable privileged Linux namespace only")
	}
	t.Setenv("QUIC_GO_DISABLE_GSO", "true")
	for _, family := range []int{4, 6} {
		for _, mtu := range []int{1500, 1280} {
			t.Run(fmt.Sprintf("ipv%d-mtu%d", family, mtu), func(t *testing.T) {
				run := func(args ...string) {
					t.Helper()
					if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
						t.Fatalf("ip %v: %v %s", args, err, out)
					}
				}
				suffix := fmt.Sprintf("%d%d%d", os.Getpid(), family, mtu)
				clientNS, serverNS := "wcc"+suffix, "wcs"+suffix
				run("netns", "add", clientNS)
				defer exec.Command("ip", "netns", "del", clientNS).Run()
				run("netns", "add", serverNS)
				defer exec.Command("ip", "netns", "del", serverNS).Run()
				clientIF, serverIF := "cwhole", "swhole"
				run("link", "add", clientIF, "type", "veth", "peer", "name", serverIF)
				run("link", "set", clientIF, "netns", clientNS)
				run("link", "set", serverIF, "netns", serverNS)
				clientIP, serverIP, network, prefix := "192.0.2.1", "192.0.2.2", "udp4", "/24"
				if family == 6 {
					clientIP, serverIP, network, prefix = "fd99:7::1", "fd99:7::2", "udp6", "/64"
				}
				for _, e := range []struct{ ns, iface, addr string }{{clientNS, clientIF, clientIP}, {serverNS, serverIF, serverIP}} {
					run("-n", e.ns, "link", "set", "lo", "up")
					run("-n", e.ns, "link", "set", e.iface, "mtu", fmt.Sprint(mtu), "up")
					args := []string{"-n", e.ns, "addr", "add", e.addr + prefix, "dev", e.iface}
					if family == 6 {
						args = append(args, "nodad")
					}
					run(args...)
				}
				cc := startCarrierCapture(t, clientNS, clientIF, clientIP, serverIP, mtu)
				defer cc.finish(t, fmt.Sprintf("ipv%d-mtu%d-client", family, mtu))
				sc := startCarrierCapture(t, serverNS, serverIF, serverIP, clientIP, mtu)
				defer sc.finish(t, fmt.Sprintf("ipv%d-mtu%d-server", family, mtu))
				a, b := testCredentials(t)
				serverTLS, err := b.PeerTLS(a.Device)
				if err != nil {
					t.Fatal(err)
				}
				serverUDP := carrierUDPAt(t, serverNS, network, net.JoinHostPort(serverIP, "0"))
				serverTransport := &quic.Transport{Conn: serverUDP}
				defer serverTransport.Close()
				listener, err := serverTransport.Listen(serverTLS, QUICConfig())
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				accepted := make(chan *quic.Conn, 1)
				errs := make(chan error, 2)
				go func() {
					conn, err := listener.Accept(ctx)
					if err != nil {
						errs <- err
					} else {
						accepted <- conn
					}
				}()
				clientUDP := carrierUDPAt(t, clientNS, network, net.JoinHostPort(clientIP, "0"))
				clientTransport := &quic.Transport{Conn: clientUDP}
				defer clientTransport.Close()
				clientTLS, err := a.PeerTLS(b.Device)
				if err != nil {
					t.Fatal(err)
				}
				client, err := clientTransport.Dial(ctx, serverUDP.LocalAddr(), clientTLS, QUICConfig())
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseWithError(0, "done")
				var server *quic.Conn
				select {
				case server = <-accepted:
				case err := <-errs:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				defer server.CloseWithError(0, "done")
				type opened struct {
					stream *quic.Stream
					max    int
				}
				openedServer := make(chan opened, 1)
				go func() {
					stream, max, err := openRouterControl(ctx, server, b.Device, a.Device, WholeIPDatagramLimit)
					if err != nil {
						errs <- err
					} else {
						openedServer <- opened{stream, max}
					}
				}()
				clientStream, maxIP, err := openRouterControl(ctx, client, a.Device, b.Device, WholeIPDatagramLimit)
				if err != nil {
					t.Fatal(err)
				}
				var serverControl opened
				select {
				case serverControl = <-openedServer:
				case err := <-errs:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				// On the 1500 path, wait for actual validated PMTU growth. On 1280, the
				// interface cannot carry a 1329-byte DATAGRAM even though hello permits it.
				if mtu == 1500 {
					// PMTU probing progresses on packet activity; an otherwise idle
					// connection can wait for its five-second keepalive between probes.
					// Drive bounded valid baseline traffic, consuming every warmup
					// before controlled frame-count checks. Capacity still requires
					// actual acknowledged probes; never force a larger initial size.
					_, warm := assemblyPacket(t, 80, 0)
					warmups := 0
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for {
						capacity, err := client.CurrentMaxDatagramPayloadSize()
						if err != nil {
							t.Fatal(err)
						}
						if capacity >= WholeIPDatagramLimit {
							t.Logf("validated whole capacity after %d baseline warmup datagrams", warmups)
							break
						}
						if warmups >= 256 {
							t.Fatal("bounded baseline traffic did not validate whole PMTU capacity")
						}
						if err := client.TrySendDatagram(warm[0]); err != nil {
							t.Fatal("baseline PMTU warmup admission", err)
						}
						got, err := server.ReceiveDatagram(ctx)
						if err != nil || !bytes.Equal(got, warm[0]) {
							t.Fatal("baseline PMTU warmup delivery", err)
						}
						warmups++
						select {
						case <-tick.C:
						case <-ctx.Done():
							t.Fatal("PMTU did not validate whole capacity", ctx.Err())
						}
					}
				}
				exchange := func(sender, receiver *quic.Conn, limit int, id uint32) CarrierIPStats {
					t.Helper()
					s := &carrierIPSender{sender: sender, negotiated: limit}
					want, _ := assemblyPacket(t, TunnelMTU, id)
					if err := s.sendIP(id, want); err != nil {
						t.Fatal(err)
					}
					assembly := IPAssembler{wholeIP: true}
					var got []byte
					count := 0
					for got == nil {
						d, err := receiver.ReceiveDatagram(ctx)
						if err != nil {
							t.Fatal(err)
						}
						count++
						got, err = assembly.Receive(d, time.Now())
						if err != nil {
							t.Fatal(err)
						}
					}
					if !bytes.Equal(got, want) {
						t.Fatal("native UDP carrier changed IP bytes")
					}
					stats := s.snapshot()
					if uint64(count) != stats.Fragments {
						t.Fatalf("actual receive frames %d versus enqueue %+v", count, stats)
					}
					t.Logf("actual receiver frame count=%d stats=%+v", count, stats)
					return stats
				}
				first := exchange(client, server, maxIP, 1)
				if mtu == 1500 && first.WholePackets != 1 {
					t.Fatal("capable path did not reduce two frames to one")
				}
				if mtu == 1280 && first.FragmentedPackets != 1 {
					t.Fatal("constrained path failed fixed baseline fallback")
				}
				// A fresh constrained hello capability remains valid on an otherwise
				// capable transport; the receiver sees exactly the two original positions.
				constrained := exchange(client, server, DatagramLimit, 2)
				if constrained.FragmentedPackets != 1 {
					t.Fatal("negotiated constraint ignored")
				}
				exchange(server, client, serverControl.max, 3)
				// Keep the established connection and remote UDP address. On the 1500
				// case, route a fresh source endpoint over a second, genuinely 1280-byte
				// veth path. The remote address remains locally owned by the server;
				// receiving it on another interface requires no proxy or NAT.
				migrationIP := clientIP
				if mtu == 1500 {
					smallClient, smallServer := "192.0.3.1", "192.0.3.2"
					bits := "/32"
					if family == 6 {
						smallClient, smallServer, bits = "fd99:8::1", "fd99:8::2", "/128"
					}
					run("link", "add", "csmall", "type", "veth", "peer", "name", "ssmall")
					run("link", "set", "csmall", "netns", clientNS)
					run("link", "set", "ssmall", "netns", serverNS)
					for _, e := range []struct{ ns, iface, addr string }{{clientNS, "csmall", smallClient}, {serverNS, "ssmall", smallServer}} {
						run("-n", e.ns, "link", "set", e.iface, "mtu", "1280", "up")
						args := []string{"-n", e.ns, "addr", "add", e.addr + prefix, "dev", e.iface}
						if family == 6 {
							args = append(args, "nodad")
						}
						run(args...)
					}
					run("-n", clientNS, "route", "add", serverIP+bits, "via", smallServer, "dev", "csmall")
					mc := startCarrierCapture(t, clientNS, "csmall", smallClient, serverIP, 1280)
					defer mc.finish(t, fmt.Sprintf("ipv%d-migration1280-client", family))
					ms := startCarrierCapture(t, serverNS, "ssmall", serverIP, smallClient, 1280)
					defer ms.finish(t, fmt.Sprintf("ipv%d-migration1280-server", family))
					migrationIP = smallClient
				}
				migratedUDP := carrierUDPAt(t, clientNS, network, net.JoinHostPort(migrationIP, "0"))
				migratedTransport := &quic.Transport{Conn: migratedUDP}
				defer migratedTransport.Close()
				path, err := client.AddPath(migratedTransport)
				if err != nil {
					t.Fatal(err)
				}
				if err := path.Probe(ctx); err != nil {
					t.Fatal(err)
				}
				if err := path.Switch(); err != nil {
					t.Fatal(err)
				}
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for client.LocalAddr().String() != migratedUDP.LocalAddr().String() {
					select {
					case <-tick.C:
					case <-ctx.Done():
						t.Fatal("path did not switch", ctx.Err())
					}
				}
				migrated := exchange(client, server, maxIP, 4)
				if migrated.FragmentedPackets != 1 || migrated.CurrentDatagram >= WholeIPDatagramLimit {
					t.Fatalf("constrained migration did not reduce capacity and preserve fallback: %+v", migrated)
				}
				exchange(server, client, serverControl.max, 5)
				deadline, _ := ctx.Deadline()
				_ = clientStream.SetWriteDeadline(deadline)
				_ = serverControl.stream.SetReadDeadline(deadline)
				if err := WriteControl(clientStream, ControlMessage{Kind: "bundle", Bundle: a.Certificate.Certificate}); err != nil {
					t.Fatal(err)
				}
				message, err := ReadControl(serverControl.stream)
				if err != nil || message.Kind != "bundle" {
					t.Fatal("control lost after path migration", err)
				}
				if client.Context().Err() != nil || server.Context().Err() != nil {
					t.Fatal("healthy neighbour closed during fallback/migration")
				}
			})
		}
	}
}
