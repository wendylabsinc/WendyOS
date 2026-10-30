package backend

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type bluetoothAgent struct {
	pb.UnimplementedWendyBluetoothServiceServer
	calls chan *pb.ConnectBluetoothPeripheralRequest
}

func (s *bluetoothAgent) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[pb.ScanBluetoothPeripheralsRequest, pb.ScanBluetoothPeripheralsResponse]) error {
	for _, connected := range []bool{false, true} {
		if err := stream.Send(&pb.ScanBluetoothPeripheralsResponse{DiscoveredDevices: []*pb.DiscoveredBluetoothPeripheral{{Name: "Sensor", Address: "00:11:22:33:44:55", Connected: connected}}}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
func (s *bluetoothAgent) ConnectBluetoothPeripheral(_ context.Context, req *pb.ConnectBluetoothPeripheralRequest) (*pb.ConnectBluetoothPeripheralResponse, error) {
	s.calls <- req
	return nil, status.Error(codes.PermissionDenied, "pairing rejected")
}
func bluetoothBackend(t *testing.T) (*Backend, *bluetoothAgent) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &bluetoothAgent{calls: make(chan *pb.ConnectBluetoothPeripheralRequest, 2)}
	pb.RegisterWendyBluetoothServiceServer(server, fake)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///bluetooth", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.session = 7
	b.life, b.stop = context.WithCancel(context.Background())
	t.Cleanup(func() { b.Shutdown(); server.Stop(); listener.Close() })
	return b, fake
}
func TestBluetoothScanKeepsLatestObservationAndStops(t *testing.T) {
	b, _ := bluetoothBackend(t)
	devices, err := scanBluetooth(context.Background(), pb.NewWendyBluetoothServiceClient(b.conn.Conn), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || !devices[0].Connected {
		t.Fatalf("expected latest observation, got %v", devices)
	}
}
func TestBluetoothMutationGuardsAndPairingFailure(t *testing.T) {
	b, fake := bluetoothBackend(t)
	for _, args := range []struct {
		session uint64
		address string
		action  int32
	}{{0, "00:11:22:33:44:55", 1}, {6, "00:11:22:33:44:55", 1}, {7, "invalid", 1}, {7, "00:11:22:33:44:55", 99}} {
		if err := b.BluetoothAction(args.session, args.address, args.action); err == nil {
			t.Fatal("invalid action accepted")
		}
	}
	select {
	case <-fake.calls:
		t.Fatal("invalid action reached device")
	default:
	}
	if err := b.BluetoothAction(7, "00:11:22:33:44:55", 1); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("pairing failure lost: %v", err)
	}
	req := <-fake.calls
	if !req.Pair || req.Trust {
		t.Fatalf("unexpected pairing/trust request: %v", req)
	}
}
func TestBluetoothScanRejectsStaleSession(t *testing.T) {
	b, _ := bluetoothBackend(t)
	for _, session := range []uint64{0, 6} {
		if _, err := b.BluetoothScan(session); err == nil {
			t.Fatal("scan accepted missing/stale session")
		}
	}
}
