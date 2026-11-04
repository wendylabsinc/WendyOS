package netmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestInternetTunnelIPFamiliesAndOuterFragmentation(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	self, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	peer, _ := meshid.ParseDevice(tenant, "33333333-3333-4333-8333-333333333333")
	source, _ := NewNetwork(self)
	defer source.Close()
	donor, _ := NewNetwork(peer)
	defer donor.Close()
	left, _ := source.AddPeer(1, peer)
	right, _ := donor.AddPeer(1, self)
	go io.Copy(left, right)
	go io.Copy(right, left)
	source.Apply([]babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), Link: 1}})
	donor.Apply([]babel.Route{{Prefix: netip.PrefixFrom(self.MeshAddress(), 128), Link: 1}})
	cfg := meshsharing.InternetConfig{Address4: "10.88.1.1", Address6: "fd2b:677a:9d11::101", MTU: 1280}
	tunnel, err := source.OpenInternetTunnel(peer.MeshAddress(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	for _, version := range []byte{4, 6} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			protocol := tcpip.TransportProtocolNumber(4)
			if version == 6 {
				protocol = 41
			}
			var queue waiter.Queue
			endpoint, err := raw.NewEndpoint(donor.stack, header.IPv6ProtocolNumber, protocol, &queue)
			if err != nil {
				t.Fatal(err)
			}
			if err := endpoint.Connect(tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom16(self.MeshAddress().As16())}); err != nil {
				t.Fatal(err)
			}
			conn := gonet.NewUDPConn(&queue, endpoint)
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			packet := make([]byte, 1280)
			if version == 4 {
				packet[0] = 0x45
				packet[8] = 64
				packet[9] = 17
				binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
				copy(packet[12:16], net.ParseIP(cfg.Address4).To4())
				copy(packet[16:20], net.ParseIP("203.0.113.8").To4())
			} else {
				packet[0] = 0x60
				packet[6] = 17
				packet[7] = 64
				binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
				copy(packet[8:24], net.ParseIP(cfg.Address6).To16())
				copy(packet[24:40], net.ParseIP("2001:db8::8").To16())
			}
			if err := tunnel.WritePacket(packet); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 65535)
			n, readErr := conn.Read(buf)
			if readErr != nil {
				t.Fatal("donor receive", readErr)
			}
			if !bytes.Equal(buf[:n], packet) {
				t.Fatal("encapsulation changed packet")
			}
			if version == 4 {
				copy(packet[16:20], net.ParseIP(cfg.Address4).To4())
				copy(packet[12:16], net.ParseIP("203.0.113.8").To4())
			} else {
				copy(packet[24:40], net.ParseIP(cfg.Address6).To16())
				copy(packet[8:24], net.ParseIP("2001:db8::8").To16())
			}
			if version == 4 {
				if _, err := conn.Write(packet); err != nil {
					t.Fatal("donor reply", err)
				}
			} else {
				// A Linux donor may fragment the outer datagram. Deliver the last
				// fragment first to exercise netstack's reassembly before raw delivery.
				for _, offset := range []int{1024, 0} {
					end := min(offset+1024, len(packet))
					fragment := make([]byte, 48+end-offset)
					fragment[0] = 0x60
					fragment[6] = 44
					fragment[7] = 64
					binary.BigEndian.PutUint16(fragment[4:6], uint16(len(fragment)-40))
					copy(fragment[8:24], peer.MeshAddress().AsSlice())
					copy(fragment[24:40], self.MeshAddress().AsSlice())
					fragment[40] = 41
					bits := uint16(offset)
					if end < len(packet) {
						bits |= 1
					}
					binary.BigEndian.PutUint16(fragment[42:44], bits)
					binary.BigEndian.PutUint32(fragment[44:48], 1234)
					copy(fragment[48:], packet[offset:end])
					donor.forward(fragment, false)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got, receiveErr := tunnel.ReadPacket(ctx)
			if receiveErr != nil {
				t.Fatal("borrower receive", receiveErr)
			}
			if !bytes.Equal(got, packet) {
				t.Fatal("decapsulation changed packet")
			}
			packet[0] = 0
			if tunnel.WritePacket(packet) == nil {
				t.Fatal("accepted malformed IP")
			}
		})
	}
	tunnel.Close()
	if _, err := tunnel.ReadPacket(context.Background()); err == nil {
		t.Fatal("closed tunnel still readable")
	}
}
