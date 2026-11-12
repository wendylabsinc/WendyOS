//go:build linux

package netmesh

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
)

// Adapt only the concrete file return type, as the production hostNetwork does.
type contractKernel struct{ *localmesh.Kernel }

func (k contractKernel) AddPeer(id babel.LinkID, peer meshid.Device) (io.ReadWriteCloser, error) {
	return k.Kernel.AddPeer(id, peer)
}

// The same identity, routes, IPv6 packets and assertions exercise the gVisor
// backend and actual Linux TUN/policy routing. No forwarding seam is replaced.
func TestPacketBackendContract(t *testing.T) {
	for _, kind := range []string{"userspace", "kernel"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "kernel" {
				if os.Getenv("WENDY_PACKET_BACKEND_NAMESPACE") != "1" {
					t.Skip("requires the disposable Linux packet-backend CI fixture")
				}
				if os.Geteuid() != 0 {
					t.Fatal("kernel fixture requires root")
				}
				for _, ns := range []string{"mnt", "net", "pid"} {
					parent := os.Getenv("WENDY_PACKET_PARENT_" + ns)
					current, err := os.Readlink("/proc/self/ns/" + ns)
					if err != nil || parent == "" || current == parent {
						t.Fatalf("kernel fixture lacks an isolated %s namespace", ns)
					}
				}
			}
			tenant, err := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
			if err != nil {
				t.Fatal(err)
			}
			otherTenant, err := meshid.ParseTenant("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
			if err != nil {
				t.Fatal(err)
			}
			device := func(tenant meshid.Tenant, digit int) meshid.Device {
				t.Helper()
				id, err := meshid.ParseDevice(tenant, fmt.Sprintf("22222222-2222-4222-8222-22222222222%d", digit))
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			self, source, target, control := device(tenant, 1), device(tenant, 2), device(tenant, 3), device(tenant, 4)
			foreign := device(otherTenant, 3)
			if foreign.MeshAddress() != target.MeshAddress() {
				t.Fatal("tenant isolation must be tested with identical routing addresses")
			}
			var network localmesh.PacketNetwork
			if kind == "userspace" {
				network, err = NewNetwork(self)
			} else {
				var kernel *localmesh.Kernel
				kernel, err = localmesh.NewKernel(self)
				network = contractKernel{kernel}
			}
			if err != nil {
				t.Fatal(err)
			}
			var readerDone, stopReader chan struct{}
			t.Cleanup(func() {
				if stopReader != nil {
					close(stopReader)
				}
				if err := network.Close(); err != nil {
					t.Errorf("close %s backend: %v", kind, err)
				}
				if readerDone != nil {
					select {
					case <-readerDone:
					case <-time.After(2 * time.Second):
						t.Error("backend close left its packet reader blocked")
					}
				}
			})
			if _, err := network.AddPeer(2, foreign); err == nil {
				t.Fatal("foreign tenant acquired a packet port")
			}
			if _, err := network.AddPeer(2, meshid.Device{}); err == nil {
				t.Fatal("invalid principal acquired a packet port")
			}
			// Reuse the rejected link ID: a refusal must not retain a TUN or slot.
			outgoing, err := network.AddPeer(2, target)
			if err != nil {
				t.Fatalf("rejected peer retained the link slot: %v", err)
			}
			incoming, err := network.AddPeer(1, source)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("foreign-tenant and invalid principals rejected; trusted link ID reused")
			type received struct {
				packet []byte
				err    error
			}
			packets := make(chan received, 64)
			readerDone, stopReader = make(chan struct{}), make(chan struct{})
			go func() {
				defer close(readerDone)
				buffer := make([]byte, localmesh.TunnelMTU)
				for {
					n, err := outgoing.Read(buffer)
					result := received{packet: append([]byte(nil), buffer[:n]...), err: err}
					select {
					case packets <- result:
					case <-stopReader:
						return
					}
					if err != nil {
						return
					}
				}
			}()
			_, nextHop, err := meshid.LinkLocalAddresses(self, target)
			if err != nil {
				t.Fatal(err)
			}
			route := func(dest meshid.Device) babel.Route {
				return babel.Route{Prefix: netip.PrefixFrom(dest.MeshAddress(), 128), Link: 2, NextHop: nextHop, Metric: 256}
			}
			apply := func(routes ...babel.Route) {
				t.Helper()
				if err := network.Apply(routes); err != nil {
					t.Fatal(err)
				}
			}
			sequence := 0
			send := func(dest meshid.Device, marker string) []byte {
				t.Helper()
				sequence++
				body, err := (&icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: 73, Seq: sequence, Data: []byte("contract:" + marker)}}).
					Marshal(icmp.IPv6PseudoHeader(net.IP(source.MeshAddress().AsSlice()), net.IP(dest.MeshAddress().AsSlice())))
				if err != nil {
					t.Fatal(err)
				}
				packet := make([]byte, 40+len(body))
				packet[0], packet[6], packet[7] = 0x60, 58, 64
				binary.BigEndian.PutUint16(packet[4:6], uint16(len(body)))
				copy(packet[8:24], source.MeshAddress().AsSlice())
				copy(packet[24:40], dest.MeshAddress().AsSlice())
				copy(packet[40:], body)
				if n, err := incoming.Write(packet); err != nil || n != len(packet) {
					t.Fatalf("inject %s: wrote %d/%d: %v", marker, n, len(packet), err)
				}
				packet[7]-- // both routers must forward exactly one hop
				return packet
			}
			denied := make(map[string]bool)
			check := func(result received) []byte {
				t.Helper()
				if result.err != nil {
					t.Fatalf("packet reader ended unexpectedly: %v", result.err)
				}
				p := result.packet
				if len(p) < 48 || p[6] != 58 || !bytes.HasPrefix(p[48:], []byte("contract:")) {
					return nil // kernel-generated neighbor/router discovery
				}
				marker := string(p[48+len("contract:"):])
				if denied[marker] {
					t.Fatalf("unrouted packet reached carrier: %s", marker)
				}
				return p
			}
			await := func(expected []byte) {
				t.Helper()
				deadline := time.NewTimer(3 * time.Second)
				defer deadline.Stop()
				for {
					select {
					case result := <-packets:
						if packet := check(result); packet != nil {
							if !bytes.Equal(packet, expected) {
								t.Fatalf("forwarded packet differs from expected bytes:\ngot %x\nwant %x", packet, expected)
							}
							return
						}
					case <-deadline.C:
						t.Fatalf("authorized packet never reached carrier: %s", expected[48:])
					}
				}
			}
			assertDenied := func(marker string) {
				t.Helper()
				denied[marker] = true
				send(target, marker)
				// An independent route on the same carrier remains usable. Its
				// packet is an activity barrier, followed by a bounded absence check.
				await(send(control, "barrier-"+marker))
				quiet := time.NewTimer(250 * time.Millisecond)
				defer quiet.Stop()
				for {
					select {
					case result := <-packets:
						if p := check(result); p != nil {
							t.Fatalf("unexpected packet after barrier: %s", p[48:])
						}
					case <-quiet.C:
						t.Logf("%s blocked while control route remained usable", marker)
						return
					}
				}
			}
			apply(route(control))
			assertDenied("absent")
			apply(route(control), route(target))
			await(send(target, "installed"))
			apply(route(control))
			assertDenied("withdrawn")
			apply(route(control), route(target))
			await(send(target, "restored"))
			unreachable := route(target)
			unreachable.Unreachable = true
			apply(route(control), unreachable)
			assertDenied("unreachable")
			apply(route(control), route(target))
			await(send(target, "restored-again"))
			t.Log("identical transit packets passed after both route restorations")
		})
	}
}
