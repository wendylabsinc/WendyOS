package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func (s *mcpServer) registerBluetoothTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("bluetooth_scan",
		mcpgo.WithDescription("Scan for Bluetooth peripherals near the connected device"),
		mcpgo.WithNumber("timeout_seconds",
			mcpgo.Description("Scan duration in seconds (default 5)"),
		),
	), s.handleBluetoothScan)

	srv.AddTool(mcpgo.NewTool("bluetooth_connect",
		mcpgo.WithDescription("Connect to a Bluetooth peripheral by address"),
		mcpgo.WithString("address",
			mcpgo.Required(),
			mcpgo.Description("Bluetooth peripheral address"),
		),
		mcpgo.WithBoolean("pair",
			mcpgo.Description("Pair the peripheral during connection"),
		),
		mcpgo.WithBoolean("trust",
			mcpgo.Description("Trust the peripheral after connecting"),
		),
	), s.handleBluetoothConnect)

	srv.AddTool(mcpgo.NewTool("bluetooth_disconnect",
		mcpgo.WithDescription("Disconnect from a Bluetooth peripheral by address"),
		mcpgo.WithString("address",
			mcpgo.Required(),
			mcpgo.Description("Bluetooth peripheral address"),
		),
	), s.handleBluetoothDisconnect)
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
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	if err := stream.Send(&agentpb.ScanBluetoothPeripheralsRequest{}); err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}

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
			return mcpgo.NewToolResultError(grpcErrString(err)), nil
		}
		for _, d := range resp.GetDiscoveredDevices() {
			seen[d.GetAddress()] = map[string]any{
				"name":        d.GetName(),
				"address":     d.GetAddress(),
				"rssi":        d.GetRssi(),
				"device_type": d.GetDeviceType(),
				"paired":      d.GetPaired(),
				"connected":   d.GetConnected(),
				"trusted":     d.GetTrusted(),
			}
		}
	}

	var devices []map[string]any
	for _, d := range seen {
		devices = append(devices, d)
	}
	if devices == nil {
		devices = []map[string]any{}
	}
	b, _ := json.MarshalIndent(devices, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleBluetoothConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	address := stringParam(req, "address")
	if address == "" {
		return mcpgo.NewToolResultError("address is required"), nil
	}
	_, err := conn.AgentService.ConnectBluetoothPeripheral(ctx, &agentpb.ConnectBluetoothPeripheralRequest{
		Address: address,
		Pair:    req.GetBool("pair", false),
		Trust:   req.GetBool("trust", false),
	})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	return mcpgo.NewToolResultText(fmt.Sprintf("connected to %s", address)), nil
}

func (s *mcpServer) handleBluetoothDisconnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	address := stringParam(req, "address")
	if address == "" {
		return mcpgo.NewToolResultError("address is required"), nil
	}
	_, err := conn.AgentService.DisconnectBluetoothPeripheral(ctx, &agentpb.DisconnectBluetoothPeripheralRequest{
		Address: address,
	})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	return mcpgo.NewToolResultText(fmt.Sprintf("disconnected from %s", address)), nil
}
