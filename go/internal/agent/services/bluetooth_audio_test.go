package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// mockBluetoothManagerWithPeripherals sends one batch of peripherals then closes.
type mockBluetoothManagerWithPeripherals struct {
	peripherals []*agentpb.DiscoveredBluetoothPeripheral
}

func (m *mockBluetoothManagerWithPeripherals) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral, 1)
	ch <- m.peripherals
	close(ch)
	return ch, nil
}
func (m *mockBluetoothManagerWithPeripherals) Connect(_ context.Context, _ string, _, _ bool) error {
	return nil
}
func (m *mockBluetoothManagerWithPeripherals) Disconnect(_ context.Context, _ string) error {
	return nil
}
func (m *mockBluetoothManagerWithPeripherals) Forget(_ context.Context, _ string) error { return nil }

// mockBluetoothManagerError returns errors for connect/disconnect/forget.
type mockBluetoothManagerError struct {
	connectErr    error
	disconnectErr error
	forgetErr     error
}

func (m *mockBluetoothManagerError) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral)
	close(ch)
	return ch, nil
}
func (m *mockBluetoothManagerError) Connect(_ context.Context, _ string, _, _ bool) error {
	return m.connectErr
}
func (m *mockBluetoothManagerError) Disconnect(_ context.Context, _ string) error {
	return m.disconnectErr
}
func (m *mockBluetoothManagerError) Forget(_ context.Context, _ string) error { return m.forgetErr }

// startAudioServer starts an AudioService gRPC server and returns a connected client.
func startAudioServer(t *testing.T) (agentpb.WendyAudioServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	svc := NewAudioService(zap.NewNop())
	agentpb.RegisterWendyAudioServiceServer(srv, svc)

	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	cl := agentpb.NewWendyAudioServiceClient(conn)
	cleanup := func() { conn.Close(); srv.Stop(); lis.Close() }
	return cl, cleanup
}

// ---------- Bluetooth tool tests ----------

func TestConnectBluetoothPeripheral(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	_, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpb.ConnectBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
	})
	if err != nil {
		t.Fatalf("ConnectBluetoothPeripheral: %v", err)
	}
}

func TestConnectBluetoothPeripheral_WithPairAndTrust(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	_, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpb.ConnectBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
		Pair:    true,
		Trust:   true,
	})
	if err != nil {
		t.Fatalf("ConnectBluetoothPeripheral with pair+trust: %v", err)
	}
}

func TestConnectBluetoothPeripheral_Error(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManagerError{connectErr: fmt.Errorf("device unreachable")},
	)
	defer cleanup()

	_, err := client.ConnectBluetoothPeripheral(context.Background(), &agentpb.ConnectBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
	})
	if err == nil {
		t.Fatal("expected error from ConnectBluetoothPeripheral")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if !strings.Contains(st.Message(), "device unreachable") {
		t.Errorf("error message = %q; want to contain 'device unreachable'", st.Message())
	}
}

func TestDisconnectBluetoothPeripheral(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	_, err := client.DisconnectBluetoothPeripheral(context.Background(), &agentpb.DisconnectBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
	})
	if err != nil {
		t.Fatalf("DisconnectBluetoothPeripheral: %v", err)
	}
}

func TestDisconnectBluetoothPeripheral_Error(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManagerError{disconnectErr: fmt.Errorf("not connected")},
	)
	defer cleanup()

	_, err := client.DisconnectBluetoothPeripheral(context.Background(), &agentpb.DisconnectBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
	})
	if err == nil {
		t.Fatal("expected error from DisconnectBluetoothPeripheral")
	}
}

func TestForgetBluetoothPeripheral(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	_, err := client.ForgetBluetoothPeripheral(context.Background(), &agentpb.ForgetBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
	})
	if err != nil {
		t.Fatalf("ForgetBluetoothPeripheral: %v", err)
	}
}

func TestForgetBluetoothPeripheral_Error(t *testing.T) {
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManagerError{forgetErr: fmt.Errorf("not paired")},
	)
	defer cleanup()

	_, err := client.ForgetBluetoothPeripheral(context.Background(), &agentpb.ForgetBluetoothPeripheralRequest{
		Address: "AA:BB:CC:DD:EE:FF",
	})
	if err == nil {
		t.Fatal("expected error from ForgetBluetoothPeripheral")
	}
}

func TestScanBluetoothPeripherals_Empty(t *testing.T) {
	// The mock returns an empty closed channel → server returns nil → client gets EOF.
	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManager{},
	)
	defer cleanup()

	stream, err := client.ScanBluetoothPeripherals(context.Background())
	if err != nil {
		t.Fatalf("ScanBluetoothPeripherals: %v", err)
	}
	// Should get EOF immediately since the mock channel is already closed.
	_, err = stream.Recv()
	if err != io.EOF {
		t.Errorf("expected EOF from empty scan, got %v", err)
	}
}

func TestScanBluetoothPeripherals_WithPeripherals(t *testing.T) {
	peripherals := []*agentpb.DiscoveredBluetoothPeripheral{
		{Address: "AA:BB:CC:DD:EE:FF", Name: "WendyDevice-1"},
		{Address: "11:22:33:44:55:66", Name: "WendyDevice-2"},
	}

	client, cleanup := startAgentServer(t,
		&mockNetworkManager{},
		&mockHardwareDiscoverer{},
		&mockBluetoothManagerWithPeripherals{peripherals: peripherals},
	)
	defer cleanup()

	stream, err := client.ScanBluetoothPeripherals(context.Background())
	if err != nil {
		t.Fatalf("ScanBluetoothPeripherals: %v", err)
	}

	// First Recv should return the batch of peripherals.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(resp.GetDiscoveredDevices()) != 2 {
		t.Fatalf("expected 2 discovered devices, got %d", len(resp.GetDiscoveredDevices()))
	}
	if resp.GetDiscoveredDevices()[0].GetAddress() != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("device[0].address = %q; want AA:BB:CC:DD:EE:FF", resp.GetDiscoveredDevices()[0].GetAddress())
	}

	// Second Recv should get EOF (channel is closed after the one batch).
	_, err = stream.Recv()
	if err != io.EOF {
		t.Errorf("expected EOF after batch, got %v", err)
	}
}

// ---------- Audio tool tests ----------

func TestListAudioDevices_ServiceAvailable(t *testing.T) {
	// ListAudioDevices calls system tools (pw-cli/pactl/arecord). In CI without
	// audio hardware it returns codes.Internal. Accept both outcomes: success
	// with 0-N devices, or an unavailable-hardware error.
	cl, cleanup := startAudioServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cl.ListAudioDevices(ctx, &agentpb.ListAudioDevicesRequest{})
	if err != nil {
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("unexpected non-gRPC error: %v", err)
		}
		// Internal is the expected code when no audio tools are installed.
		if st.Code() != codes.Internal {
			t.Fatalf("unexpected gRPC status %v: %v", st.Code(), err)
		}
		t.Logf("ListAudioDevices returned Internal (no audio hardware in CI): %v", st.Message())
		return
	}
	// If it succeeds, devices may be nil or empty — just verify it's a valid response.
	t.Logf("ListAudioDevices returned %d device(s)", len(resp.GetDevices()))
}

func TestParseALSAOutput_InputDevices(t *testing.T) {
	// Sample output from `arecord -l` on a system with one sound card.
	output := `**** List of CAPTURE Hardware Devices ****
card 0: PCH [HDA Intel PCH], device 0: ALC3246 Analog [ALC3246 Analog]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 0: PCH [HDA Intel PCH], device 2: ALC3246 Alt Analog [ALC3246 Alt Analog]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`

	devices := parseALSAOutput(output, agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT)
	if len(devices) == 0 {
		t.Fatal("expected at least one device from ALSA output")
	}
	for _, d := range devices {
		if d.GetName() == "" {
			t.Error("device name should not be empty")
		}
		if d.GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT {
			t.Errorf("device type = %v; want INPUT", d.GetType())
		}
	}
	t.Logf("parsed %d ALSA input device(s)", len(devices))
}

func TestParseALSAOutput_OutputDevices(t *testing.T) {
	output := `**** List of PLAYBACK Hardware Devices ****
card 0: PCH [HDA Intel PCH], device 0: ALC3246 Analog [ALC3246 Analog]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`

	devices := parseALSAOutput(output, agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT)
	if len(devices) == 0 {
		t.Fatal("expected at least one device from ALSA output")
	}
	if devices[0].GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT {
		t.Errorf("device type = %v; want OUTPUT", devices[0].GetType())
	}
}

func TestParseALSAOutput_Empty(t *testing.T) {
	devices := parseALSAOutput("", agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT)
	if len(devices) != 0 {
		t.Errorf("expected 0 devices from empty output, got %d", len(devices))
	}
}

func TestParseALSAOutput_NoSoundcards(t *testing.T) {
	output := "arecord: device_list:272: no soundcards found...\n"
	devices := parseALSAOutput(output, agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT)
	if len(devices) != 0 {
		t.Errorf("expected 0 devices when no soundcards, got %d", len(devices))
	}
}
