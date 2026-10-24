package cloudrelay

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestEveryLoopbackPortService(t *testing.T) {
	for port := uint32(1); port <= 65535; port++ {
		for _, protocol := range []string{"tcp", "udp"} {
			service, err := PortService(protocol, port)
			if err != nil {
				t.Fatal(err)
			}
			transport, gotPort, err := serviceTarget(service)
			if err != nil || gotPort != port {
				t.Fatalf("%s: %d, %v", service, gotPort, err)
			}
			d := &pb.DialInstruction{Transport: transport, ServiceDescriptor: []byte(service)}
			if protocol == "tcp" {
				d.Host, d.Port = "127.0.0.1", port
			} else {
				d.AuthorizedDatagramDestinations = []*pb.AuthorizedDatagramDestination{{DestinationId: 1, LoopbackPort: port}}
			}
			if err := validateService(d); err != nil {
				t.Fatalf("%s: %v", service, err)
			}
		}
	}
}

func TestInvalidPortServices(t *testing.T) {
	for _, service := range []string{
		"tcp-0", "udp-65536", "tcp-4294967296", "udp--1", "tcp-+22", "tcp-022",
		"TCP-22", "udp-", "tcp-22-extra", "tcp-127.0.0.1:22", "udp- 53",
		"tcp-２２", "udp-53\n", "tcp-22/localhost", "udp-0", "tcp-65536", "torrent",
	} {
		t.Run(service, func(t *testing.T) {
			if _, _, err := serviceTarget(service); err == nil {
				t.Fatal("accepted malformed service")
			}
		})
	}
	for _, port := range []uint32{0, 65536, ^uint32(0)} {
		for _, protocol := range []string{"tcp", "udp"} {
			if _, err := PortService(protocol, port); err == nil {
				t.Fatal("accepted invalid port")
			}
		}
	}
}

func TestPortServiceTargetBinding(t *testing.T) {
	tcp := &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: 3007, ServiceDescriptor: []byte("tcp-3007")}
	udp := &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_DATAGRAM, ServiceDescriptor: []byte("udp-5353"), AuthorizedDatagramDestinations: []*pb.AuthorizedDatagramDestination{{DestinationId: 1, LoopbackPort: 5353}}}
	for _, host := range []string{"169.254.169.254", "10.0.0.1", "localhost", "example.com", "", "::1", "127.0.0.2", "127.1", "0:0:0:0:0:ffff:7f00:1"} {
		d := proto.Clone(tcp).(*pb.DialInstruction)
		d.Host = host
		if validateService(d) == nil {
			t.Fatalf("accepted host %q", host)
		}
	}
	for name, mutate := range map[string]func(*pb.DialInstruction){
		"port": func(d *pb.DialInstruction) { d.AuthorizedDatagramDestinations[0].LoopbackPort++ },
		"id":   func(d *pb.DialInstruction) { d.AuthorizedDatagramDestinations[0].DestinationId++ },
		"extra": func(d *pb.DialInstruction) {
			d.AuthorizedDatagramDestinations = append(d.AuthorizedDatagramDestinations, &pb.AuthorizedDatagramDestination{DestinationId: 2, LoopbackPort: 53})
		},
		"host":      func(d *pb.DialInstruction) { d.Host = "127.0.0.1" },
		"tcp port":  func(d *pb.DialInstruction) { d.Port = 5353 },
		"transport": func(d *pb.DialInstruction) { d.Transport = pb.DialTransport_DIAL_TRANSPORT_TCP },
		"missing":   func(d *pb.DialInstruction) { d.AuthorizedDatagramDestinations = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := proto.Clone(udp).(*pb.DialInstruction)
			mutate(d)
			if validateService(d) == nil {
				t.Fatal("accepted mismatched UDP target")
			}
		})
	}
	tcp.Port++
	if validateService(tcp) == nil {
		t.Fatal("accepted mismatched TCP port")
	}
}

func TestDatagramFrameBounds(t *testing.T) {
	for _, d := range []*pb.TunnelDatagram{nil, {DestinationId: 0}, {DestinationId: 2}, {DestinationId: 1, Payload: make([]byte, 65508)}} {
		if validDatagram(d) {
			t.Fatal("accepted invalid datagram")
		}
	}
	for _, size := range []int{0, 65507} {
		if !validDatagram(&pb.TunnelDatagram{DestinationId: 1, Payload: make([]byte, size)}) {
			t.Fatal("rejected valid datagram")
		}
	}
}

func TestUDPFlowLimitsAndExpiry(t *testing.T) {
	// Pre-existing connected sockets avoid starting reader goroutines in this
	// deterministic accounting test. The end-to-end test covers socket traffic.
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	now := time.Now()
	r := &udpRelay{port: uint32(peer.LocalAddr().(*net.UDPAddr).Port), flows: map[uint32]*udpFlow{}}
	for id := uint32(0); id < maxDatagramFlows; id++ {
		conn, err := net.DialUDP("udp4", nil, peer.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		r.flows[id] = &udpFlow{conn: conn, lastSeen: now}
	}
	if _, err := r.flow(100, now); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("flow cap: %v", err)
	}
	if _, err := r.flow(0, now); err != nil {
		t.Fatalf("existing flow at cap: %v", err)
	}
	for range maxFlowCreations {
		r.creations = append(r.creations, now.Add(datagramIdle))
	}
	// At idle expiry sockets are removed, but the independent creation cap remains.
	if _, err := r.flow(100, now.Add(datagramIdle)); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("creation cap: %v", err)
	}
	if len(r.flows) != 0 {
		t.Fatalf("expired sockets retained: %d", len(r.flows))
	}
}

type oldCloud struct {
	pb.UnimplementedTunnelAuthorizationServiceServer
}

func (oldCloud) RequestTunnel(context.Context, *pb.RequestTunnelRequest) (*pb.RequestTunnelResponse, error) {
	return nil, status.Error(codes.NotFound, "unknown service")
}

func TestOldCloudRefusesPortService(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, oldCloud{})
	go func() { _ = srv.Serve(listener) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	sign := func(b []byte) ([]byte, error) { return b, nil }
	asset := "00000000-0000-4000-8000-000000000042"
	var unsupported *UnsupportedServiceError
	if _, err := OpenTCP(ctx, ctx, conn, nil, asset, "tcp-3000", sign); !errors.As(err, &unsupported) || unsupported.Service != "tcp-3000" {
		t.Fatalf("TCP: %v", err)
	}
	if _, err := OpenUDP(ctx, ctx, conn, nil, asset, 5353, sign); err == nil || !strings.Contains(err.Error(), `Cloud does not offer tunnel service "udp-5353"`) {
		t.Fatalf("UDP: %v", err)
	}
}
