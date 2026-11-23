//go:build linux

package rtps

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

const isolationDomain = 201

func isolationEndpoint(wired bool) Endpoint {
	prefix := GUIDPrefix{0x71, 0x19, 0x22}
	name := "loopback"
	if wired {
		prefix[11] = 1
		name = "wired"
	}
	return Endpoint{GUID: GUID{Prefix: prefix, EntityID: 0x107}, Topic: "rt/" + name + "/image", Type: "sensor_msgs::msg::dds_::Image_"}
}

// Synthetic RTPS datagrams traverse real Linux interfaces and production
// receive/discovery/subscription paths. There is no camera or middleware mock.
func isolationPublisher(ctx context.Context, ifaceName, address string, ep Endpoint) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(address)})
	if err != nil {
		return err
	}
	defer conn.Close()
	packet := ipv4.NewPacketConn(conn)
	if err := packet.SetMulticastInterface(iface); err != nil {
		return err
	}
	if err := packet.SetMulticastLoopback(true); err != nil {
		return err
	}
	if err := packet.SetMulticastTTL(1); err != nil {
		return err
	}
	group := &net.UDPAddr{IP: net.ParseIP(spdpMulticastAddr), Port: spdpMulticastPort(isolationDomain)}
	spdp := spdpData(ep.GUID.Prefix, 90, udpv4Locator(net.ParseIP(address), conn.LocalAddr().(*net.UDPAddr).Port))
	publication := publicationData(ep, 1)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for sn := SequenceNumber(1); ; sn++ {
		datagram := buildMessage(ep.GUID.Prefix,
			buildData(entitySPDPReader, entitySPDPWriter, sn, spdp.Payload),
			buildData(entityUnknown, entitySEDPPubWriter, sn, publication.Payload),
			buildData(entityUnknown, ep.GUID.EntityID, sn, append([]byte{0, 1, 0, 0}, []byte(ep.Topic+":fixture-bytes")...)))
		if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		if _, err := conn.WriteToUDP(datagram, group); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func TestMulticastPacketsStayOnTheirJoinedInterface(t *testing.T) {
	if os.Getenv("WENDY_RTPS_ISOLATION") != "1" {
		t.Skip("requires test-rtps-multicast-isolation.sh on the existing disposable Linux network runner")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_RTPS_PARENT_" + kind)
		if err != nil || parent == "" || current == parent || os.Geteuid() != 0 {
			t.Fatalf("missing private %s namespace", kind)
		}
	}
	if os.Getenv("WENDY_RTPS_WIRE_PEER") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := isolationPublisher(ctx, "peer0", "10.83.0.2", isolationEndpoint(true)); err != nil {
			t.Fatal(err)
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type observation struct {
		participant *Participant
		expected    Endpoint
		done        chan struct{}
		samples     int
	}
	var receivers []*observation
	for _, iface := range []string{"lo", "wire0"} {
		participant, err := NewParticipant(Config{DomainID: isolationDomain, Interface: iface, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		r := &observation{participant: participant, expected: isolationEndpoint(iface == "wire0"), done: make(chan struct{})}
		receivers = append(receivers, r)
		go func() { defer close(r.done); participant.Run(ctx) }()
		t.Cleanup(func() {
			participant.Close()
			select {
			case <-r.done:
			case <-time.After(2 * time.Second):
				t.Error("owned participant did not join its receive/announcement workers")
			}
		})
	}
	localDone := make(chan error, 1)
	go func() { localDone <- isolationPublisher(ctx, "lo", "127.0.0.1", isolationEndpoint(false)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-localDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("owned loopback publisher did not stop")
		}
	})

	peerCtx, peerCancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(peerCancel)
	peer := exec.CommandContext(peerCtx, "ip", "netns", "exec", "wire-peer", os.Args[0], "-test.run=^TestMulticastPacketsStayOnTheirJoinedInterface$", "-test.v", "-test.count=1", "-test.timeout=7s")
	peer.Env = append(os.Environ(), "WENDY_RTPS_WIRE_PEER=1")
	peer.WaitDelay = time.Second
	var peerLog bytes.Buffer
	peer.Stdout, peer.Stderr = &peerLog, &peerLog
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	// ip netns exec replaces itself with this binary. Await its ordinary bounded
	// completion, then inspect logs; never signal a saved/reaped numeric PID.
	t.Cleanup(func() {
		if err := peer.Wait(); err != nil {
			t.Errorf("owned wire publisher: %v\n%s", err, peerLog.String())
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range receivers {
			for _, endpoint := range r.participant.Endpoints() {
				if endpoint.GUID != r.expected.GUID || endpoint.Topic != r.expected.Topic || endpoint.Type != r.expected.Type {
					t.Fatalf("CROSS_INTERFACE_DISCOVERY: %s admitted %s (%s)", r.expected.Topic, endpoint.Topic, endpoint.GUID)
				}
				if err := r.participant.Subscribe(endpoint); err != nil {
					t.Fatal(err)
				}
			}
			for {
				select {
				case sample := <-r.participant.Samples():
					want := append([]byte{0, 1, 0, 0}, []byte(r.expected.Topic+":fixture-bytes")...)
					if sample.Writer != r.expected.GUID || sample.SN < 1 || !bytes.Equal(sample.Payload, want) {
						t.Fatalf("CROSS_INTERFACE_SAMPLE: got %+v, want %s", sample, r.expected.GUID)
					}
					r.samples++
				default:
					goto nextReceiver
				}
			}
		nextReceiver:
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, r := range receivers {
		if endpoints := r.participant.Endpoints(); len(endpoints) != 1 || endpoints[0].GUID != r.expected.GUID || r.samples < 20 {
			t.Fatalf("positive discovery/sample path missing: %s endpoints=%+v samples=%d stats=%+v", r.expected.Topic, endpoints, r.samples, r.participant.Stats())
		}
		t.Logf("isolated %s: %d exact samples; stats=%+v", r.expected.Topic, r.samples, r.participant.Stats())
	}
	t.Log("RTPS_INTERFACE_PACKET_ISOLATION_VERIFIED")
}
