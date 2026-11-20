//go:build js && wasm

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall/js"
	"time"

	"github.com/coder/websocket"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

type deviceStream struct {
	stream grpc.ClientStream
	method protoreflect.MethodDescriptor
	cancel context.CancelFunc
}

func (b *bridge) connectLocalDevice() (any, error) {
	origin := js.Global().Get("location").Get("origin").String()
	if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
		return nil, errors.New("Device panel requires an HTTP(S) origin")
	}
	endpoint := "ws" + strings.TrimPrefix(origin, "http") + "/api/agent"
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.close != nil {
		b.close()
	}
	if b.conn != nil {
		_ = b.conn.Close()
	}
	b.conn, b.deviceLocal, b.deviceStreams = nil, false, nil
	life, stop := context.WithCancel(context.Background())
	conn, err := grpc.NewClient("passthrough:///device-admin", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		ws, _, err := websocket.Dial(ctx, endpoint, nil)
		if err != nil {
			return nil, err
		}
		return websocket.NetConn(life, ws, websocket.MessageBinary), nil
	}), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)))
	if err != nil {
		stop()
		return nil, err
	}
	agent := grpcclient.NewFromConn(conn)
	ctx, cancel := context.WithTimeout(life, 15*time.Second)
	defer cancel()
	version, err := agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		stop()
		_ = conn.Close()
		return nil, fmt.Errorf("Connecting to this device: %w", err)
	}
	b.conn, b.life, b.close, b.deviceLocal = agent, life, stop, true
	b.deviceStreams = make(map[string]deviceStream)
	return json.RawMessage(protojson.Format(version)), nil
}

func deviceMethod(service, method string) (protoreflect.MethodDescriptor, error) {
	// Only agent protocols compiled with this CLI. No Cloud APIs or reflection
	// supplied by a remote server can introduce a new request type here.
	if !strings.HasPrefix(service, "wendy.agent.") {
		return nil, errors.New("Not a device service")
	}
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil, err
	}
	svc, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, errors.New("Unknown device service")
	}
	m := svc.Methods().ByName(protoreflect.Name(method))
	if m == nil {
		return nil, errors.New("Unknown device operation")
	}
	return m, nil
}

func (b *bridge) deviceRequest(r request) (any, error) {
	b.mu.Lock()
	conn, life, local := b.conn, b.life, b.deviceLocal
	b.mu.Unlock()
	if !local || conn == nil {
		return nil, errors.New("Connect to the device panel first")
	}
	if r.Method == "device-sha256" {
		sum := sha256.Sum256(r.bytes)
		return hex.EncodeToString(sum[:]), nil
	}
	var p struct {
		Service  string          `json:"service"`
		Method   string          `json:"method"`
		Message  json.RawMessage `json:"message"`
		ID       string          `json:"stream"`
		KeepOpen bool            `json:"keepOpen"`
	}
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return nil, err
	}
	if r.Method == "device-stream-stop" || r.Method == "device-stream-send" {
		b.mu.Lock()
		stream, ok := b.deviceStreams[p.ID]
		if r.Method == "device-stream-stop" {
			delete(b.deviceStreams, p.ID)
		}
		b.mu.Unlock()
		if !ok {
			return true, nil
		}
		if r.Method == "device-stream-stop" {
			stream.cancel()
			return true, nil
		}
		msg := dynamicpb.NewMessage(stream.method.Input())
		if err := protojson.Unmarshal(p.Message, msg); err != nil {
			return nil, err
		}
		return true, stream.stream.SendMsg(msg)
	}
	m, err := deviceMethod(p.Service, p.Method)
	if err != nil {
		return nil, err
	}
	if r.Method == "device-schema" {
		return deviceFields(m.Input(), 0), nil
	}
	if r.Method != "device-rpc" {
		return nil, errors.New("Unknown device operation")
	}
	input := dynamicpb.NewMessage(m.Input())
	if len(p.Message) == 0 {
		p.Message = []byte("{}")
	}
	if err := protojson.Unmarshal(p.Message, input); err != nil {
		return nil, err
	}
	path := "/" + p.Service + "/" + p.Method
	if !m.IsStreamingClient() && !m.IsStreamingServer() {
		ctx, cancel := context.WithTimeout(life, 35*time.Second)
		defer cancel()
		output := dynamicpb.NewMessage(m.Output())
		if err := conn.Conn.Invoke(ctx, path, input, output); err != nil {
			return nil, err
		}
		return deviceJSON(output), nil
	}
	if p.ID == "" {
		return nil, errors.New("A stream ID is required")
	}
	ctx, cancel := context.WithCancel(life)
	stream, err := conn.Conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: m.IsStreamingServer(), ClientStreams: m.IsStreamingClient()}, path)
	if err != nil {
		cancel()
		return nil, err
	}
	if err = stream.SendMsg(input); err != nil {
		cancel()
		return nil, err
	}
	if !m.IsStreamingClient() || !p.KeepOpen {
		_ = stream.CloseSend()
	}
	b.mu.Lock()
	if previous, exists := b.deviceStreams[p.ID]; exists {
		previous.cancel()
	}
	b.deviceStreams[p.ID] = deviceStream{stream, m, cancel}
	b.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if current, ok := b.deviceStreams[p.ID]; ok && current.stream == stream {
				delete(b.deviceStreams, p.ID)
			}
		}()
		for {
			output := dynamicpb.NewMessage(m.Output())
			err := stream.RecvMsg(output)
			if err != nil {
				if ctx.Err() == nil {
					detail := ""
					if err != io.EOF {
						detail = err.Error()
					}
					event("device-stream", map[string]any{"stream": p.ID, "done": true, "error": detail})
				}
				return
			}
			event("device-stream", map[string]any{"stream": p.ID, "message": deviceJSON(output)})
		}
	}()
	return true, nil
}

// Form metadata uses the same descriptors as request encoding. This keeps enum
// choices and optional/nested settings in step with the shipped CLI protocol.
func deviceFields(message protoreflect.MessageDescriptor, depth int) []map[string]any {
	fields := []map[string]any{}
	if depth > 4 {
		return fields
	}
	for i := 0; i < message.Fields().Len(); i++ {
		f := message.Fields().Get(i)
		field := map[string]any{"name": f.JSONName(), "kind": f.Kind().String(), "list": f.IsList(), "map": f.IsMap()}
		if one := f.ContainingOneof(); one != nil && !one.IsSynthetic() {
			field["oneof"] = string(one.Name())
		}
		if f.Enum() != nil {
			values := []string{}
			for j := 0; j < f.Enum().Values().Len(); j++ {
				values = append(values, string(f.Enum().Values().Get(j).Name()))
			}
			field["options"] = values
		}
		if f.Message() != nil {
			field["fields"] = deviceFields(f.Message(), depth+1)
		}
		fields = append(fields, field)
	}
	return fields
}

func deviceJSON(message proto.Message) json.RawMessage {
	data, _ := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(message)
	return data
}
