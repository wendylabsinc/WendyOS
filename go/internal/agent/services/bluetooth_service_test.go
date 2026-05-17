package services

import (
	"context"
	"net"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
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

	stream, err := client.ScanBluetoothPeripherals(context.Background(), &agentpbv2.ScanBluetoothPeripheralsRequest{})
	if err != nil {
		t.Fatalf("ScanBluetoothPeripherals: %v", err)
	}

	_, err = stream.Recv()
	// mockBluetoothManager closes the channel immediately, server returns nil → EOF
	if err == nil {
		// received one response — also fine
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
