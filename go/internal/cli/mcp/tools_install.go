package mcp

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

// SetInstallationBackend must be called before Start.
func (s *mcpServer) SetInstallationBackend(backend onboarding.Backend) { s.installation = backend }

func (s *mcpServer) registerInstallationTools(srv *server.MCPServer) {
	plan := append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Plan initial WendyOS or agent installation: artifact, erase scope, prerequisites and CLI arguments. No disk writes. For supported image installs, os_install_start creates a resumable job."),
	}, installationOptionSchema()...)
	plan = append(plan, readOnly()...)
	plan = append(plan, openWorld()...)
	srv.AddTool(mcpgo.NewTool("os_install_plan", plan...), s.handleInstallPlan)
	s.registerInstallationJobTools(srv)
	drives := []mcpgo.ToolOption{mcpgo.WithDescription("List host installation drives, excluding the system drive. Match path, model and capacity before planning a write; removable status is not authorization to erase.")}
	drives = append(drives, readOnly()...)
	drives = append(drives, localOnly()...)
	srv.AddTool(mcpgo.NewTool("os_list_drives", drives...), s.handleInstallDrives)
	verify := []mcpgo.ToolOption{
		mcpgo.WithDescription("Verify first boot at an explicit target without changing the active connection. Checks agent, optional OS/board/public key and enrollment. Application readiness remains unchecked."),
		mcpgo.WithString("address", mcpgo.Required(), mcpgo.Description("Explicit hostname, IP:port, or cloud selector; never chooses a default device")),
		mcpgo.WithString("expected_os_version"), mcpgo.WithString("expected_device_type"), mcpgo.WithString("expected_public_key"),
		mcpgo.WithBoolean("require_enrollment"),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(60), mcpgo.DefaultNumber(15)),
	}
	verify = append(verify, readOnly()...)
	verify = append(verify, openWorld()...)
	srv.AddTool(mcpgo.NewTool("os_install_verify", verify...), s.handleInstallVerify)
}

func installationOptionSchema() []mcpgo.ToolOption {
	return []mcpgo.ToolOption{
		mcpgo.WithString("device_type", mcpgo.Required(), mcpgo.Description("Exact board: raspberry-pi-3/4/5, jetson-orin-nano, jetson-agx-orin, jetson-agx-thor, unitree-g1, or linux-desktop")),
		mcpgo.WithString("carrier", mcpgo.Description("Required for Jetson: developer-kit. Custom robot carriers need vendor instructions, not generic Jetson images.")),
		mcpgo.WithString("version", mcpgo.Description("Exact WendyOS version; omitted resolves latest stable")),
		mcpgo.WithString("storage", mcpgo.Enum("sd", "nvme", "emmc")),
		mcpgo.WithString("drive", mcpgo.Description("Exact host disk path from os_list_drives; only for raw-media writes")),
		mcpgo.WithBoolean("rootfs_only", mcpgo.Description("Orin raw-media write only; leaves QSPI firmware unchanged")),
	}
}

func installationOptions(req mcpgo.CallToolRequest) (onboarding.Options, error) {
	o := onboarding.Options{
		DeviceType: strings.TrimSpace(stringParam(req, "device_type")), Carrier: stringParam(req, "carrier"), Version: stringParam(req, "version"),
		Storage: stringParam(req, "storage"), Drive: stringParam(req, "drive"),
	}
	if o.DeviceType == "" {
		return o, errors.New("device_type is required")
	}
	if v, present := req.GetArguments()["rootfs_only"]; present {
		var valid bool
		o.RootfsOnly, valid = v.(bool)
		if !valid {
			return o, errors.New("rootfs_only must be a boolean")
		}
	}
	return o, nil
}

func (s *mcpServer) handleInstallPlan(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.installation.Plan == nil {
		return errResult(errCodeUnsupported, "installation planning is unavailable in this host; use wendy install --help"), nil
	}
	opts, err := installationOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	p, err := s.installation.Plan(ctx, opts)
	if err != nil {
		code := errCodeInvalidArgument
		if errors.Is(err, onboarding.ErrArtifactUnavailable) {
			code = errCodeInternal
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			code = errCodeTimeout
		}
		return errResult(code, err.Error()), nil
	}
	return okResult(p), nil
}

func (s *mcpServer) handleInstallDrives(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.installation.Drives == nil {
		return errResult(errCodeUnsupported, "host drive enumeration is unavailable"), nil
	}
	drives, err := s.installation.Drives()
	if err != nil {
		return errResult(errCodeInternal, err.Error()), nil
	}
	return okList("drives", drives), nil
}

func (s *mcpServer) handleInstallVerify(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return errResult(errCodeUnsupported, "verify from a host MCP session without WENDY_AGENT_SOCKET overriding the explicit target"), nil
	}
	timeout, err := ros2Int(req, "timeout_seconds", 15, 1, 60)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	v, err := onboarding.Verify(ctx, onboarding.VerifyOptions{
		Address: stringParam(req, "address"), OSVersion: stringParam(req, "expected_os_version"),
		DeviceType: stringParam(req, "expected_device_type"), PublicKey: stringParam(req, "expected_public_key"),
		RequireEnrollment: req.GetBool("require_enrollment", false), Timeout: time.Duration(timeout) * time.Second,
	}, s.connectFn)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	r := okResult(v)
	r.IsError = !v.Verified
	return r, nil
}
