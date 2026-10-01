package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestSimulatorCreateRejectsInvalidInputsBeforeBackend(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetSimulatorBackend(SimulatorBackend{Create: func(context.Context, SimulatorCreateOptions) (*SimulatorInfo, error) {
		t.Fatal("invalid creation reached backend")
		return nil, nil
	}})
	for _, args := range []map[string]any{
		{}, {"name": "../escape"}, {"name": 7},
		{"name": "dev", "profile": "unsupported"}, {"name": "dev", "profile": false},
		{"name": "dev", "disk_gib": 1.5}, {"name": "dev", "disk_gib": 0}, {"name": "dev", "disk_gib": 1025},
		{"name": "dev", "image": "local.wic", "version": "v1"},
		{"name": "dev", "version": "../v1"}, {"name": "dev", "image": true},
	} {
		r, err := s.handleSimulatorCreate(context.Background(), callToolReq("simulator_create", args))
		if err != nil || !r.IsError || structuredMap(t, r)["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("args %v: %v %v", args, r, err)
		}
	}
}

func TestSimulatorToolsWorkWithoutConnectionAndKeepReadinessUnknown(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetSimulatorBackend(SimulatorBackend{
		Create: func(_ context.Context, o SimulatorCreateOptions) (*SimulatorInfo, error) {
			if o.Name != "dev" || o.Profile != "generic" || o.DiskGiB != 16 {
				t.Fatalf("defaults: %+v", o)
			}
			return &SimulatorInfo{Name: o.Name, Device: "vm:dev", State: "stopped"}, nil
		},
		Stop: func(_ context.Context, name string, force bool, timeout time.Duration) (*SimulatorInfo, error) {
			if name != "dev" || force || timeout != 60*time.Second {
				t.Fatalf("stop unexpectedly cuts power: %q %v %s", name, force, timeout)
			}
			return &SimulatorInfo{Name: name, State: "stopped"}, nil
		},
	})
	r, err := s.handleSimulatorCreate(context.Background(), callToolReq("simulator_create", map[string]any{"name": "dev"}))
	if err != nil || r.IsError {
		t.Fatalf("create: %v %v", r, err)
	}
	m := structuredMap(t, r)
	if m["readiness"] != "not_checked" || m["device"] != "vm:dev" || m["next_step"] != "device_connect" {
		t.Fatalf("create implied readiness or lost selector: %v", m)
	}
	r, err = s.handleSimulatorStop(context.Background(), callToolReq("simulator_stop", map[string]any{"name": "dev"}))
	if err != nil || r.IsError || s.GetConn() != nil {
		t.Fatalf("stop: %v %v", r, err)
	}
}

func TestSimulatorStopAndDeleteValidateBeforeMutation(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetSimulatorBackend(SimulatorBackend{
		Stop: func(context.Context, string, bool, time.Duration) (*SimulatorInfo, error) {
			t.Fatal("invalid stop reached backend")
			return nil, nil
		},
		Delete: func(context.Context, string) error { t.Fatal("invalid delete reached backend"); return nil },
	})
	for _, args := range []map[string]any{
		{}, {"name": "dev", "force": "true"}, {"name": "dev", "timeout_seconds": 0},
		{"name": "dev", "timeout_seconds": 1.5}, {"name": "vm:dev"},
	} {
		r, _ := s.handleSimulatorStop(context.Background(), callToolReq("simulator_stop", args))
		if !r.IsError {
			t.Fatalf("accepted %v", args)
		}
	}
	r, _ := s.handleSimulatorDelete(context.Background(), callToolReq("simulator_delete", map[string]any{"name": "../dev"}))
	if !r.IsError {
		t.Fatal("accepted traversal name")
	}
}

func TestSimulatorListReturnsCompleteBoundedRows(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetSimulatorBackend(SimulatorBackend{List: func(context.Context) ([]SimulatorInfo, error) {
		var items []SimulatorInfo
		for i := range 30 {
			items = append(items, SimulatorInfo{Name: fmt.Sprintf("dev-%d", i), State: "unknown", Error: strings.Repeat("x", 200)})
		}
		return items, nil
	}})
	r, err := s.handleSimulatorList(context.Background(), callToolReq("simulator_list", map[string]any{"max_bytes": 1024, "max_results": 10}))
	if err != nil || r.IsError {
		t.Fatalf("list: %v %v", r, err)
	}
	m := structuredMap(t, r)
	rows := listPayload(t, r, "simulators")
	if len(rows) == 0 || len(rows) >= 10 || m["truncated"] != true || m["omitted"] != 30-len(rows) || len(toolResultText(t, r)) > 1024 {
		t.Fatalf("unexpected bounded result: %s", toolResultText(t, r))
	}
}

func TestSimulatorToolsHostGuardAndAnnotations(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetSimulatorBackend(SimulatorBackend{List: func(context.Context) ([]SimulatorInfo, error) {
		t.Fatal("on-device session touched host VM store")
		return nil, nil
	}})
	t.Setenv("WENDY_AGENT_SOCKET", "/fake/socket")
	r, _ := s.handleSimulatorList(context.Background(), callToolReq("simulator_list", nil))
	if !r.IsError || structuredMap(t, r)["error_code"] != "UNSUPPORTED" {
		t.Fatal(r)
	}
	srv := server.NewMCPServer("test", "test")
	s.registerSimulatorTools(srv)
	for _, name := range []string{"simulator_stop", "simulator_delete"} {
		if !*srv.GetTool(name).Tool.Annotations.DestructiveHint {
			t.Fatalf("%s does not disclose disruption", name)
		}
	}
	if !*srv.GetTool("simulator_list").Tool.Annotations.ReadOnlyHint || *srv.GetTool("simulator_create").Tool.Annotations.DestructiveHint {
		t.Fatal("wrong read/create annotations")
	}
}

func TestSimulatorStopClearsOnlyTheStoppedConnection(t *testing.T) {
	for _, changeConnection := range []bool{false, true} {
		s := New(&config.Config{}, nil)
		s.SetConn(&grpcclient.AgentConnection{SimulatorName: "dev"})
		other := &grpcclient.AgentConnection{SimulatorName: "other"}
		s.SetSimulatorBackend(SimulatorBackend{Stop: func(context.Context, string, bool, time.Duration) (*SimulatorInfo, error) {
			if changeConnection {
				s.SetConn(other)
			}
			return &SimulatorInfo{Name: "dev", State: "stopped"}, nil
		}})
		r, err := s.handleSimulatorStop(context.Background(), callToolReq("simulator_stop", map[string]any{"name": "dev"}))
		if err != nil || r.IsError {
			t.Fatalf("stop: %v %v", r, err)
		}
		if changeConnection && s.GetConn() != other {
			t.Fatal("stop cleared a newly selected connection")
		}
		if !changeConnection && s.GetConn() != nil {
			t.Fatal("stopped simulator still reported connected")
		}
	}
}
