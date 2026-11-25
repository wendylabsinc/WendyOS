package backend

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type bluetoothAgent struct {
	pb.UnimplementedWendyBluetoothServiceServer
	calls       chan *pb.ConnectBluetoothPeripheralRequest
	scanStopped chan struct{}
}

func (s *bluetoothAgent) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[pb.ScanBluetoothPeripheralsRequest, pb.ScanBluetoothPeripheralsResponse]) error {
	defer func() { s.scanStopped <- struct{}{} }()
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
	fake := &bluetoothAgent{calls: make(chan *pb.ConnectBluetoothPeripheralRequest, 2), scanStopped: make(chan struct{}, 1)}
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
	// NewClient dials lazily. Establish the fixture transport before starting
	// a short scan budget, so runner scheduling/setup does not consume it.
	setup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(setup, state) {
			t.Fatalf("Bluetooth fixture transport not ready: %v", setup.Err())
		}
	}
	return b, fake
}
func TestBluetoothScanKeepsLatestObservationAndStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, fake := bluetoothBackend(t)
		devices, err := scanBluetooth(context.Background(), pb.NewWendyBluetoothServiceClient(b.conn.Conn), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if len(devices) != 1 || !devices[0].Connected {
			t.Fatalf("expected latest observation, got %v", devices)
		}
		select {
		case <-fake.scanStopped:
		case <-time.After(time.Second):
			t.Fatal("scan stream did not stop after its deadline")
		}
	})
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

// A deadline can have elapsed before context's timer callback updates Err.
// gRPC's RST_STREAM handler uses the deadline's timestamp in that interval.
type bluetoothDeadlineSnapshot struct {
	context.Context
	deadline time.Time
}

func (c bluetoothDeadlineSnapshot) Deadline() (time.Time, bool) { return c.deadline, true }

func TestBluetoothScanDeadlineCompletion(t *testing.T) {
	now := time.Date(2026, time.November, 5, 0, 0, 0, 0, time.UTC)
	background := context.Background()
	pending := bluetoothDeadlineSnapshot{background, now}
	expired, cancelExpired := context.WithDeadline(background, time.Time{})
	defer cancelExpired()
	canceled, cancel := context.WithCancel(background)
	cancel()
	deadlineErr := status.Error(codes.DeadlineExceeded, "stream terminated by RST_STREAM with error code: CANCEL")
	for _, tt := range []struct {
		name         string
		parent, scan context.Context
		received     bool
		err          error
		want         bool
	}{
		{"callback pending at deadline", background, pending, true, deadlineErr, true},
		{"callback pending after deadline", background, bluetoothDeadlineSnapshot{background, now.Add(-time.Second)}, true, deadlineErr, true},
		{"callback completed", background, expired, true, deadlineErr, true},
		{"early remote deadline", background, bluetoothDeadlineSnapshot{background, now.Add(time.Second)}, true, deadlineErr, false},
		{"no observations", background, pending, false, deadlineErr, false},
		{"permission failure", background, pending, true, status.Error(codes.PermissionDenied, "denied"), false},
		{"transport cancellation", background, pending, true, status.Error(codes.Canceled, "connection closed"), false},
		{"caller canceled", canceled, pending, true, deadlineErr, false},
		{"caller deadline callback pending", pending, pending, true, deadlineErr, false},
		{"caller deadline callback completed", expired, pending, true, deadlineErr, false},
		{"caller still active", bluetoothDeadlineSnapshot{background, now.Add(time.Second)}, pending, true, deadlineErr, true},
		{"no scan deadline", background, background, true, deadlineErr, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := bluetoothScanDeadlineReached(tt.parent, tt.scan, tt.received, tt.err, now); got != tt.want {
				t.Fatalf("deadline completion = %v, want %v (scan Err=%v)", got, tt.want, tt.scan.Err())
			}
		})
	}
}
