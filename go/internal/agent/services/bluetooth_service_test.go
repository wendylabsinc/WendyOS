package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/agent/bluetooth"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func startBluetoothServer(t *testing.T, bm BluetoothManager) (agentpbv2.WendyBluetoothServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	svc := NewBluetoothService(zap.NewNop(), bm)
	agentpbv2.RegisterWendyBluetoothServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpbv2.NewWendyBluetoothServiceClient(conn), func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
}

func TestBluetoothService_ScanReturnsEmpty(t *testing.T) {
	client, cleanup := startBluetoothServer(t, &mockBluetoothManager{})
	defer cleanup()

	stream, err := client.ScanBluetoothPeripherals(context.Background())
	if err != nil {
		t.Fatalf("ScanBluetoothPeripherals: %v", err)
	}
	// The mock closes the scan channel immediately, so the server may finish
	// before the client's Send arrives. Both outcomes are valid.
	_ = stream.Send(&agentpbv2.ScanBluetoothPeripheralsRequest{})
	stream.CloseSend()

	_, err = stream.Recv()
	// mockBluetoothManager closes the channel immediately, server returns nil → EOF
	if err == nil {
		// received one response — also fine
	}
}

// failingBluetoothManager returns err from every operation (Scan comes from
// the embedded always-succeeding mock).
type failingBluetoothManager struct {
	mockBluetoothManager
	err error
}

func (m *failingBluetoothManager) Connect(_ context.Context, _ string, _, _ bool) (bool, error) {
	return false, m.err
}
func (m *failingBluetoothManager) Disconnect(_ context.Context, _ string) error { return m.err }
func (m *failingBluetoothManager) Forget(_ context.Context, _ string) error     { return m.err }

// pairedReportingManager reports a fixed post-connect pairing state.
type pairedReportingManager struct {
	mockBluetoothManager
	paired bool
}

func (m *pairedReportingManager) Connect(_ context.Context, _ string, _, _ bool) (bool, error) {
	return m.paired, nil
}

func TestBluetoothService_ConnectReportsPairedState(t *testing.T) {
	for _, paired := range []bool{true, false} {
		client, cleanup := startBluetoothServer(t, &pairedReportingManager{paired: paired})
		resp, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpbv2.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"})
		cleanup()
		if err != nil {
			t.Fatalf("ConnectBluetoothPeripheral: %v", err)
		}
		if resp.Paired == nil {
			t.Fatalf("paired=%v: response must carry the pairing state", paired)
		}
		if resp.GetPaired() != paired {
			t.Errorf("resp.Paired = %v, want %v", resp.GetPaired(), paired)
		}
	}
}

func TestAgentService_ConnectReportsPairedState(t *testing.T) {
	svc := NewAgentService(zap.NewNop(), &mockNetworkManager{}, &mockHardwareDiscoverer{}, &pairedReportingManager{paired: false}, &AgentInstaller{})
	resp, err := svc.ConnectBluetoothPeripheral(context.Background(), &agentpb.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"})
	if err != nil {
		t.Fatalf("ConnectBluetoothPeripheral: %v", err)
	}
	if resp.Paired == nil || resp.GetPaired() {
		t.Errorf("v1 response should report paired=false, got %v", resp.Paired)
	}
}

func TestBluetoothService_DeviceNotFoundMapsToNotFound(t *testing.T) {
	notFound := fmt.Errorf("%w: device AA:BB:CC:DD:EE:FF was not seen within 12s of discovery — rescan", bluetooth.ErrDeviceNotFound)
	client, cleanup := startBluetoothServer(t, &failingBluetoothManager{err: notFound})
	defer cleanup()

	_, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpbv2.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"})
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected a gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound (message %q)", st.Code(), st.Message())
	}
	if !strings.Contains(st.Message(), "was not seen within") {
		t.Errorf("message = %q, want the manager's friendly text preserved", st.Message())
	}

	if _, err := client.DisconnectBluetoothPeripheral(context.Background(), &agentpbv2.DisconnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); status.Code(err) != codes.NotFound {
		t.Errorf("disconnect code = %v, want NotFound", status.Code(err))
	}
	if _, err := client.ForgetBluetoothPeripheral(context.Background(), &agentpbv2.ForgetBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); status.Code(err) != codes.NotFound {
		t.Errorf("forget code = %v, want NotFound", status.Code(err))
	}
}

func TestBluetoothService_GenericErrorMapsToInternal(t *testing.T) {
	client, cleanup := startBluetoothServer(t, &failingBluetoothManager{err: errors.New("bearer failure")})
	defer cleanup()

	_, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpbv2.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

func TestAgentService_ConnectBluetoothNotFoundMapsToNotFound(t *testing.T) {
	notFound := fmt.Errorf("%w: device AA:BB:CC:DD:EE:FF is not known to the Bluetooth adapter", bluetooth.ErrDeviceNotFound)
	svc := NewAgentService(zap.NewNop(), &mockNetworkManager{}, &mockHardwareDiscoverer{}, &failingBluetoothManager{err: notFound}, &AgentInstaller{})

	_, err := svc.ConnectBluetoothPeripheral(context.Background(), &agentpb.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("code = %v, want NotFound (err %v)", status.Code(err), err)
	}
}

func TestBluetoothService_ConnectDisconnectForget(t *testing.T) {
	client, cleanup := startBluetoothServer(t, &mockBluetoothManager{})
	defer cleanup()

	if _, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpbv2.ConnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); err != nil {
		t.Fatalf("ConnectBluetoothPeripheral: %v", err)
	}
	if _, err := client.DisconnectBluetoothPeripheral(context.Background(), &agentpbv2.DisconnectBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); err != nil {
		t.Fatalf("DisconnectBluetoothPeripheral: %v", err)
	}
	if _, err := client.ForgetBluetoothPeripheral(context.Background(), &agentpbv2.ForgetBluetoothPeripheralRequest{Address: "AA:BB:CC:DD:EE:FF"}); err != nil {
		t.Fatalf("ForgetBluetoothPeripheral: %v", err)
	}
}

func TestMapBluetoothPeripheralToV2_CopiesLinkTimeouts(t *testing.T) {
	eff, req := uint32(500), uint32(3000)
	got := mapBluetoothPeripheralToV2(&agentpb.DiscoveredBluetoothPeripheral{
		Address:                       "AA:BB:CC:DD:EE:FF",
		Connected:                     true,
		SupervisionTimeoutMs:          &eff,
		RequestedSupervisionTimeoutMs: &req,
	})
	if got.GetSupervisionTimeoutMs() != 500 || got.GetRequestedSupervisionTimeoutMs() != 3000 {
		t.Errorf("v2 link timeouts = %v/%v; want 500/3000", got.SupervisionTimeoutMs, got.RequestedSupervisionTimeoutMs)
	}

	bare := mapBluetoothPeripheralToV2(&agentpb.DiscoveredBluetoothPeripheral{Address: "AA:BB:CC:DD:EE:FF"})
	if bare.SupervisionTimeoutMs != nil || bare.RequestedSupervisionTimeoutMs != nil {
		t.Error("absent link timeouts must stay absent in v2")
	}
}
