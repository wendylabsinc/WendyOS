package commands

import (
	"context"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// mockAgentServiceClient implements agentpb.WendyAgentServiceClient for tests.
// Populate only the fields relevant to the behaviour under test; all other
// methods return zero values.
type mockAgentServiceClient struct {
	updateAgentStream grpc.BidiStreamingClient[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]
	updateAgentErr    error
}

func (m *mockAgentServiceClient) UpdateAgent(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse], error) {
	return m.updateAgentStream, m.updateAgentErr
}

func (m *mockAgentServiceClient) RunContainer(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.RunContainerRequest, agentpb.RunContainerResponse], error) {
	return nil, nil
}
func (m *mockAgentServiceClient) GetAgentVersion(_ context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) ListWiFiNetworks(_ context.Context, _ *agentpb.ListWiFiNetworksRequest, _ ...grpc.CallOption) (*agentpb.ListWiFiNetworksResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) ConnectToWiFi(_ context.Context, _ *agentpb.ConnectToWiFiRequest, _ ...grpc.CallOption) (*agentpb.ConnectToWiFiResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) GetWiFiStatus(_ context.Context, _ *agentpb.GetWiFiStatusRequest, _ ...grpc.CallOption) (*agentpb.GetWiFiStatusResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) DisconnectWiFi(_ context.Context, _ *agentpb.DisconnectWiFiRequest, _ ...grpc.CallOption) (*agentpb.DisconnectWiFiResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) ListHardwareCapabilities(_ context.Context, _ *agentpb.ListHardwareCapabilitiesRequest, _ ...grpc.CallOption) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) ScanBluetoothPeripherals(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.ScanBluetoothPeripheralsRequest, agentpb.ScanBluetoothPeripheralsResponse], error) {
	return nil, nil
}
func (m *mockAgentServiceClient) ConnectBluetoothPeripheral(_ context.Context, _ *agentpb.ConnectBluetoothPeripheralRequest, _ ...grpc.CallOption) (*agentpb.ConnectBluetoothPeripheralResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) DisconnectBluetoothPeripheral(_ context.Context, _ *agentpb.DisconnectBluetoothPeripheralRequest, _ ...grpc.CallOption) (*agentpb.DisconnectBluetoothPeripheralResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) ForgetBluetoothPeripheral(_ context.Context, _ *agentpb.ForgetBluetoothPeripheralRequest, _ ...grpc.CallOption) (*agentpb.ForgetBluetoothPeripheralResponse, error) {
	return nil, nil
}
func (m *mockAgentServiceClient) UpdateOS(_ context.Context, _ *agentpb.UpdateOSRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.UpdateOSResponse], error) {
	return nil, nil
}

// fakeUpdateClientStream implements grpc.BidiStreamingClient for testing
// deviceUpdateUpload. All sent messages accumulate in sent. Recv returns an
// Updated response once, then io.EOF.
type fakeUpdateClientStream struct {
	sent    []*agentpb.UpdateAgentRequest
	recvPos int
}

func (f *fakeUpdateClientStream) Send(r *agentpb.UpdateAgentRequest) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeUpdateClientStream) Recv() (*agentpb.UpdateAgentResponse, error) {
	if f.recvPos == 0 {
		f.recvPos++
		return &agentpb.UpdateAgentResponse{
			ResponseType: &agentpb.UpdateAgentResponse_Updated_{
				Updated: &agentpb.UpdateAgentResponse_Updated{},
			},
		}, nil
	}
	return nil, io.EOF
}

func (f *fakeUpdateClientStream) CloseSend() error              { return nil }
func (f *fakeUpdateClientStream) Context() context.Context      { return context.Background() }
func (f *fakeUpdateClientStream) Header() (metadata.MD, error)  { return nil, nil }
func (f *fakeUpdateClientStream) Trailer() metadata.MD          { return nil }
func (f *fakeUpdateClientStream) SendMsg(any) error             { return nil }
func (f *fakeUpdateClientStream) RecvMsg(any) error             { return nil }
