package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type ProjectValidationOptions struct {
	ProjectPath string
	BuildType   string
	Device      *agentpb.GetAgentVersionResponse
	DeviceError string
}

type ProjectDiagnostic struct {
	Severity string `json:"severity"`
	Field    string `json:"field"`
	Message  string `json:"message"`
	Fix      string `json:"fix"`
}

type ProjectBuild struct {
	Service string `json:"service,omitempty"`
	Type    string `json:"type"`
	File    string `json:"file,omitempty"`
}

type ProjectCompatibilityCheck struct {
	Field   string `json:"field"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type ProjectCompatibility struct {
	Status string                      `json:"status"`
	Scope  string                      `json:"scope"`
	Checks []ProjectCompatibilityCheck `json:"checks"`
}

type ProjectValidation struct {
	ProjectPath   string               `json:"project_path"`
	AppID         string               `json:"app_id,omitempty"`
	BuildType     string               `json:"build_type,omitempty"`
	Valid         bool                 `json:"valid"`
	Diagnostics   []ProjectDiagnostic  `json:"diagnostics"`
	Builds        []ProjectBuild       `json:"builds"`
	Compatibility ProjectCompatibility `json:"compatibility"`
}

type ProjectBackend struct {
	Validate func(context.Context, ProjectValidationOptions) (*ProjectValidation, error)
}

func (s *mcpServer) SetProjectBackend(backend ProjectBackend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project = backend
}

func (s *mcpServer) registerProjectTools(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Validate local project configuration and build inputs without building or running hooks. Reports actionable errors, warnings, and known or unknown device compatibility; works offline."),
		mcpgo.WithString("project_path", mcpgo.Required(), mcpgo.Description("Project directory containing wendy.json or a Compose file")),
		mcpgo.WithString("build_type", mcpgo.Enum("docker", "compose", "swift", "python"), mcpgo.Description("Optional build system override, matching run")),
		mcpgo.WithBoolean("check_device", mcpgo.DefaultBool(true), mcpgo.Description("Use the current connection for reported platform/hardware checks when available")),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1024), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384), mcpgo.Description("JSON response budget with omitted detail counts")),
	}
	opts = append(opts, readOnly()...)
	opts = append(opts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("project_validate", opts...), s.handleProjectValidate)
}

func (s *mcpServer) handleProjectValidate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	path := stringParam(req, "project_path")
	if strings.TrimSpace(path) == "" {
		return errResult(errCodeInvalidArgument, "project_path is required"), nil
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	buildType := stringParam(req, "build_type")
	if value, exists := req.GetArguments()["build_type"]; exists {
		if _, ok := value.(string); !ok || (buildType != "docker" && buildType != "compose" && buildType != "swift" && buildType != "python") {
			return errResult(errCodeInvalidArgument, "build_type must be docker, compose, swift, or python"), nil
		}
	}
	checkDevice := true
	if value, exists := req.GetArguments()["check_device"]; exists {
		var ok bool
		checkDevice, ok = value.(bool)
		if !ok {
			return errResult(errCodeInvalidArgument, "check_device must be a boolean"), nil
		}
	}
	maxBytes, err := ros2Int(req, "max_bytes", 16384, 1024, 1000000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	s.mu.RLock()
	backend := s.project
	s.mu.RUnlock()
	if backend.Validate == nil {
		return errResult(errCodeUnsupported, "project validation is unavailable in this MCP host"), nil
	}
	opts := ProjectValidationOptions{ProjectPath: path, BuildType: buildType}
	if checkDevice {
		if conn := s.GetConn(); conn != nil {
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			opts.Device, err = conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
			cancel()
			if err != nil {
				opts.Device = nil
				opts.DeviceError = "device information unavailable: " + grpcErrString(err)
			}
		} else {
			opts.DeviceError = "no device connected"
		}
	} else {
		opts.DeviceError = "device check was not requested"
	}
	validation, err := backend.Validate(ctx, opts)
	if err != nil {
		return errResultf(errCodeInternal, "validating project: %v", err), nil
	}
	if validation == nil {
		return errResult(errCodeInternal, "project validation returned no result"), nil
	}
	return projectValidationResult(validation, maxBytes), nil
}

func projectValidationResult(validation *ProjectValidation, maxBytes int) *mcpgo.CallToolResult {
	// Keep the verdict and useful finding rows even when detailed build or
	// compatibility lists would crowd them out.
	metadata := map[string]any{"project_path": validation.ProjectPath, "app_id": validation.AppID, "build_type": validation.BuildType, "valid": validation.Valid, "builds": validation.Builds, "compatibility": validation.Compatibility}
	data, _ := json.Marshal(metadata)
	if len(data) > maxBytes/2 {
		metadata["builds_omitted"] = len(validation.Builds)
		delete(metadata, "builds")
		metadata["compatibility"] = map[string]any{"status": validation.Compatibility.Status, "scope": validation.Compatibility.Scope, "checks_omitted": len(validation.Compatibility.Checks)}
	}
	// A filesystem path alone can exceed a small caller budget. Its identity
	// is already an input; preserve validation findings first.
	data, _ = json.Marshal(metadata)
	if len(data) > maxBytes/2 {
		delete(metadata, "project_path")
		metadata["details_omitted"] = true
	}
	return okRowsBounded("diagnostics", validation.Diagnostics, metadata, maxBytes, len(validation.Diagnostics))
}
