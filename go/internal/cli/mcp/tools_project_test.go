package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type projectInfoClient struct {
	agentpb.WendyAgentServiceClient
	response *agentpb.GetAgentVersionResponse
	err      error
	calls    int
}

func (c *projectInfoClient) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	c.calls++
	return c.response, c.err
}

func TestProjectValidateOfflineAndDeviceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		connected, skip, unreachable bool
	}{
		{name: "offline"}, {name: "connected", connected: true}, {name: "skipped", connected: true, skip: true}, {name: "unreachable", connected: true, unreachable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&config.Config{}, nil)
			device := &projectInfoClient{response: &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"}}
			if tc.unreachable {
				device.response = nil
				device.err = errors.New("offline")
			}
			if tc.connected {
				s.SetConn(&grpcclient.AgentConnection{AgentService: device})
			}
			var observed ProjectValidationOptions
			s.SetProjectBackend(ProjectBackend{Validate: func(_ context.Context, opts ProjectValidationOptions) (*ProjectValidation, error) {
				observed = opts
				return &ProjectValidation{ProjectPath: opts.ProjectPath, Valid: false, Diagnostics: []ProjectDiagnostic{{Severity: "error", Field: "appId", Message: "missing", Fix: "Set appId"}}, Compatibility: ProjectCompatibility{Status: "unknown"}}, nil
			}})
			args := map[string]any{"project_path": ".", "build_type": "docker"}
			if tc.skip {
				args["check_device"] = false
			}
			result, err := s.handleProjectValidate(context.Background(), callToolReq("project_validate", args))
			if err != nil || result.IsError || structuredMap(t, result)["valid"] != false {
				t.Fatalf("validation: %+v %v", result, err)
			}
			if !filepath.IsAbs(observed.ProjectPath) || observed.BuildType != "docker" {
				t.Fatalf("options: %+v", observed)
			}
			if tc.connected && !tc.skip && !tc.unreachable {
				if observed.Device != device.response {
					t.Fatal("device facts lost")
				}
			} else if observed.Device != nil || observed.DeviceError == "" {
				t.Fatalf("unknown device facts not explicit: %+v", observed)
			}
			if tc.skip && device.calls != 0 {
				t.Fatal("device check ran despite false")
			}
		})
	}
}

func TestProjectValidateRejectsInvalidArgumentsAndMissingBackend(t *testing.T) {
	s := New(&config.Config{}, nil)
	for _, args := range []map[string]any{{}, {"project_path": " "}, {"project_path": ".", "build_type": "bad"}, {"project_path": ".", "check_device": "yes"}, {"project_path": ".", "max_bytes": 1024.5}} {
		result, _ := s.handleProjectValidate(context.Background(), callToolReq("project_validate", args))
		if !result.IsError || structuredMap(t, result)["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("accepted %+v", args)
		}
	}
	result, _ := s.handleProjectValidate(context.Background(), callToolReq("project_validate", map[string]any{"project_path": "."}))
	if !result.IsError || structuredMap(t, result)["error_code"] != "UNSUPPORTED" {
		t.Fatalf("missing backend: %+v", result)
	}
	s.SetProjectBackend(ProjectBackend{Validate: func(context.Context, ProjectValidationOptions) (*ProjectValidation, error) {
		return nil, errors.New("failure")
	}})
	result, _ = s.handleProjectValidate(context.Background(), callToolReq("project_validate", map[string]any{"project_path": "."}))
	if !result.IsError {
		t.Fatal("backend failure was successful")
	}
}

func TestProjectValidateBudgetPreservesVerdictAndFindings(t *testing.T) {
	validation := &ProjectValidation{ProjectPath: strings.Repeat("long-path/", 300), AppID: "robot", BuildType: "docker", Valid: false, Compatibility: ProjectCompatibility{Status: "unknown", Scope: "platform"}, Diagnostics: []ProjectDiagnostic{{Severity: "error", Field: "appId", Message: "missing", Fix: "Set appId"}, {Severity: "warning", Field: "config", Message: strings.Repeat("x", 3000)}}}
	for range 100 {
		validation.Builds = append(validation.Builds, ProjectBuild{Type: "docker", File: strings.Repeat("path", 100)})
	}
	result := projectValidationResult(validation, 1024)
	data, _ := json.Marshal(result.StructuredContent)
	if len(data) > 1024 {
		t.Fatalf("response has %d bytes", len(data))
	}
	out := structuredMap(t, result)
	if out["valid"] != false || out["app_id"] != "robot" || out["omitted"] != 1 || len(out["diagnostics"].([]ProjectDiagnostic)) != 1 {
		t.Fatalf("lost verdict/finding: %+v", out)
	}
}

func TestProjectValidateReadOnlyAnnotation(t *testing.T) {
	s := New(&config.Config{}, nil)
	protocol := server.NewMCPServer("test", "1")
	s.registerProjectTools(protocol)
	tool := protocol.ListTools()["project_validate"].Tool
	if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatalf("annotations: %+v", tool.Annotations)
	}
}
