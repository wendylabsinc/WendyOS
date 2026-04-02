package commands

import (
	"context"

	"google.golang.org/grpc"

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
