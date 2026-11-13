package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type kernelLogAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	request   *agentpb.DumpKernelLogRequest
	oversized bool
}

func (a *kernelLogAgent) DumpKernelLog(req *agentpb.DumpKernelLogRequest, stream agentpb.WendyAgentService_DumpKernelLogServer) error {
	a.request = req
	for i := 0; i < 5; i++ {
		if err := stream.Send(&agentpb.DumpKernelLogResponse{Records: []*agentpb.KernelLogRecord{{TimestampUs: int64(i), Level: 3, Message: fmt.Sprintf("record-%d", i)}}}); err != nil {
			return err
		}
	}
	if a.oversized {
		return stream.Send(&agentpb.DumpKernelLogResponse{Records: []*agentpb.KernelLogRecord{{Message: strings.Repeat("x", 20000)}}})
	}
	return nil
}

func TestDeviceOSLogsTailIsPassiveAndBounded(t *testing.T) {
	a := &kernelLogAgent{oversized: true}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(g, a)
	go g.Serve(ln)
	t.Cleanup(g.Stop)
	c, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := New(&config.Config{}, nil)
	s.SetConn(grpcclient.NewFromConn(c))
	r, err := s.handleDeviceOSLogs(context.Background(), callToolReq("device_os_logs", map[string]any{"max_records": 2}))
	if err != nil || r.IsError {
		t.Fatalf("%v %v", r, err)
	}
	if a.request.Follow == nil || a.request.GetFollow() {
		t.Fatal("kernel dump must explicitly disable follow")
	}
	encoded, _ := json.Marshal(r.StructuredContent)
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	rows := out["logs"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["message"] != "record-3" || out["complete"] != true || out["tail_omitted"] != float64(4) {
		t.Fatalf("unexpected tail: %+v", out)
	}
}

func TestAgentUpdateUsesPinnedReconnectClosure(t *testing.T) {
	s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("generic reconnect loses broker pin")
		return nil, nil
	})
	next := &grpcclient.AgentConnection{Host: "same"}
	pinned := false
	current := &grpcclient.AgentConnection{Host: "same", Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) { pinned = true; return next, nil }}
	s.setConnection(current, "cloud", commandTarget{Device: "same", Selector: "cloud://endpoint/org/1/asset/2", Transport: "cloud", BrokerURL: "private-broker:443"})
	s.updateAgentCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		return `{"status":"success","version":"2"}`, false, nil
	}
	r, err := s.handleDeviceUpdateAgent(context.Background(), callToolReq("device_update_agent", nil))
	if err != nil || r.IsError || !pinned || s.GetConn() != next {
		t.Fatalf("lost reconnect pin: %v %v", r, err)
	}
}

func TestAgentUpdatePinsTargetAndReconnectsWithoutOSUpdate(t *testing.T) {
	selector := "cloud://cloud.test/tenant/one/asset/two"
	next := &grpcclient.AgentConnection{Host: "same-device"}
	s := New(&config.Config{}, func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
		if address != selector {
			t.Fatalf("reconnected wrong device %s", address)
		}
		return next, nil
	})
	target := commandTarget{Device: "same-device", Selector: selector, Transport: "cloud", CloudGRPC: "cloud.test", BrokerURL: "broker.test"}
	s.setConnection(&grpcclient.AgentConnection{Host: "same-device"}, "cloud", target)
	s.updateAgentCommandFn = func(_ context.Context, args []string, got commandTarget, limit int) (string, bool, error) {
		want := []string{"--json", "--device", selector, "device", "update", "--nightly"}
		if !reflect.DeepEqual(args, want) || got != target {
			t.Fatalf("unscoped update: %v %+v", args, got)
		}
		return "diagnostic\n{\"status\":\"success\",\"version\":\"2.0\",\"message\":\"verified\"}\n", false, nil
	}
	r, err := s.handleDeviceUpdateAgent(context.Background(), callToolReq("device_update_agent", map[string]any{"nightly": true}))
	if err != nil || r.IsError {
		t.Fatalf("%v %v", r, err)
	}
	out := structuredMap(t, r)
	if out["version"] != "2.0" || out["os_update"] != "not_requested" || out["connection"] != "connected" || s.GetConn() != next {
		t.Fatalf("%+v", out)
	}
	_, _, got := s.connectionSnapshot()
	if got != target {
		t.Fatalf("lost cloud identity %+v", got)
	}
}

func TestAgentUpdateDoesNotOverwriteConcurrentDeviceSelection(t *testing.T) {
	s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("must not reconnect replaced session")
		return nil, nil
	})
	s.SetConn(&grpcclient.AgentConnection{Host: "old", Addr: "old:50051"})
	next := &grpcclient.AgentConnection{Host: "new", Addr: "new:50051"}
	s.updateAgentCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		s.SetConn(next)
		return `{"status":"up-to-date","version":"2"}`, false, nil
	}
	r, err := s.handleDeviceUpdateAgent(context.Background(), callToolReq("device_update_agent", nil))
	if err != nil || r.IsError || s.GetConn() != next {
		t.Fatalf("overwrote session %v %v", r, err)
	}
}

func TestAgentUpdateRequiresVerifiedCLIResult(t *testing.T) {
	for _, output := range []string{"", "success", `{"status":"failed"}`} {
		s := New(&config.Config{}, nil)
		s.SetConn(&grpcclient.AgentConnection{Host: "old", Addr: "old:50051"})
		s.updateAgentCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) { return output, false, nil }
		r, err := s.handleDeviceUpdateAgent(context.Background(), callToolReq("device_update_agent", nil))
		if err != nil || !r.IsError || structuredMap(t, r)["status"] != "unconfirmed" {
			t.Fatalf("accepted unverified output %q: %v %v", output, r, err)
		}
	}
	if _, err := parseAgentUpdateResult(strings.Repeat("x", 100)); err == nil {
		t.Fatal("accepted non-JSON output")
	}
}
