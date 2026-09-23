package mcp

import (
	"context"
	"fmt"
	"io"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerBluetoothTools(srv *server.MCPServer) {
	scanOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Scan for Bluetooth peripherals near the connected device"),
		mcpgo.WithNumber("timeout_seconds",
			mcpgo.Description("Scan duration in seconds (default 5)"),
		),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)")),
	}
	scanOpts = append(scanOpts, readOnly()...)
	scanOpts = append(scanOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("bluetooth_scan", scanOpts...), s.handleBluetoothScan)

	connectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Connect to a Bluetooth peripheral by address"),
		mcpgo.WithString("address",
			mcpgo.Required(),
			mcpgo.Description("Bluetooth peripheral address, e.g. AA:BB:CC:DD:EE:FF"),
		),
		mcpgo.WithBoolean("pair",
			mcpgo.Description("Pair the peripheral during connection"),
		),
		mcpgo.WithBoolean("trust",
			mcpgo.Description("Trust the peripheral after connecting"),
		),
	}
	connectOpts = append(connectOpts, mutating()...)
	connectOpts = append(connectOpts, openWorld()...)
	connectOpts = append(connectOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("bluetooth_connect", connectOpts...), s.handleBluetoothConnect)

	disconnectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Disconnect from a Bluetooth peripheral by address"),
		mcpgo.WithString("address",
			mcpgo.Required(),
			mcpgo.Description("Bluetooth peripheral address, e.g. AA:BB:CC:DD:EE:FF"),
		),
	}
	disconnectOpts = append(disconnectOpts, destructive()...)
	disconnectOpts = append(disconnectOpts, openWorld()...)
	disconnectOpts = append(disconnectOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("bluetooth_disconnect", disconnectOpts...), s.handleBluetoothDisconnect)
}

func (s *mcpServer) handleBluetoothScan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	timeoutSec := intParam(req, "timeout_seconds", 5)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	stream, err := conn.AgentService.ScanBluetoothPeripherals(ctx)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	if err := stream.Send(&agentpb.ScanBluetoothPeripheralsRequest{}); err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	_ = stream.CloseSend()

	seen := map[string]map[string]any{}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		for _, d := range resp.GetDiscoveredDevices() {
			dev := map[string]any{
				"name":        d.GetName(),
				"address":     d.GetAddress(),
				"rssi":        d.GetRssi(),
				"device_type": d.GetDeviceType(),
				"paired":      d.GetPaired(),
				"connected":   d.GetConnected(),
				"trusted":     d.GetTrusted(),
			}
			if d.SupervisionTimeoutMs != nil {
				dev["supervision_timeout_ms"] = d.GetSupervisionTimeoutMs()
			}
			if d.RequestedSupervisionTimeoutMs != nil {
				dev["requested_supervision_timeout_ms"] = d.GetRequestedSupervisionTimeoutMs()
			}
			seen[d.GetAddress()] = dev
		}
	}

	var devices []map[string]any
	for _, d := range seen {
		devices = append(devices, d)
	}
	return okListBounded("devices", devices, intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleBluetoothConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	address := stringParam(req, "address")
	if address == "" {
		return errResult(errCodeInvalidArgument, "address is required"), nil
	}
	_, err := conn.AgentService.ConnectBluetoothPeripheral(ctx, &agentpb.ConnectBluetoothPeripheralRequest{
		Address: address,
		Pair:    req.GetBool("pair", false),
		Trust:   req.GetBool("trust", false),
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okText(fmt.Sprintf("connected to %s", address)), nil
}

func (s *mcpServer) handleBluetoothDisconnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	address := stringParam(req, "address")
	if address == "" {
		return errResult(errCodeInvalidArgument, "address is required"), nil
	}
	_, err := conn.AgentService.DisconnectBluetoothPeripheral(ctx, &agentpb.DisconnectBluetoothPeripheralRequest{
		Address: address,
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	return okText(fmt.Sprintf("disconnected from %s", address)), nil
}
