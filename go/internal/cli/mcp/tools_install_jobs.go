package mcp

import (
	"context"
	"errors"
	"os"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func (s *mcpServer) registerInstallationJobTools(srv *server.MCPServer) {
	start := append([]mcpgo.ToolOption{mcpgo.WithDescription("Create a persistent installation job and return physical setup instructions. Does not erase anything. Resume after the user completes the step; only supported image installs can execute.")}, installationOptionSchema()...)
	start = append(start, mutating()...)
	start = append(start, openWorld()...)
	srv.AddTool(mcpgo.NewTool("os_install_start", start...), s.handleInstallStart)
	status := []mcpgo.ToolOption{
		mcpgo.WithDescription("Read a saved installation job, including its phase, progress, instructions and first-boot verification. Works after MCP restarts."),
		mcpgo.WithString("job_id", mcpgo.Required()),
	}
	status = append(status, readOnly()...)
	status = append(status, localOnly()...)
	srv.AddTool(mcpgo.NewTool("os_install_status", status...), s.handleInstallStatus)
	resume := []mcpgo.ToolOption{
		mcpgo.WithDescription("Resume a saved install after a physical step. Probes hardware before asking for erase authorization; confirm only the returned target with the user's approval. Repeated calls cannot repeat a completed write. After flashing, supply an explicit address to verify boot."),
		mcpgo.WithString("job_id", mcpgo.Required()),
		mcpgo.WithString("target_id", mcpgo.Description("Exact target fingerprint from this job's latest readiness probe")),
		mcpgo.WithBoolean("confirm_erase", mcpgo.Description("User approved erasing this exact target and the recorded erase scope")),
		mcpgo.WithBoolean("confirm_internal", mcpgo.Description("User additionally approved a non-removable drive when required")),
		mcpgo.WithString("address", mcpgo.Description("Explicit first-boot address, used only after writing")),
		mcpgo.WithString("expected_public_key", mcpgo.Description("Optional expected first-boot identity")),
	}
	resume = append(resume, destructive()...)
	resume = append(resume, idempotent()...)
	resume = append(resume, openWorld()...)
	srv.AddTool(mcpgo.NewTool("os_install_resume", resume...), s.handleInstallResume)
}

func (s *mcpServer) handleInstallStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.installation.Start == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return errResult(errCodeUnsupported, "installation jobs require a host MCP session with the installation backend"), nil
	}
	opts, err := installationOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	job, err := s.installation.Start(ctx, onboarding.StartOptions{Options: opts})
	return installJobResult(job, err), nil
}

func (s *mcpServer) handleInstallStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.installation.Status == nil {
		return errResult(errCodeUnsupported, "installation jobs are unavailable on this host"), nil
	}
	id := strings.TrimSpace(stringParam(req, "job_id"))
	if id == "" {
		return errResult(errCodeInvalidArgument, "job_id is required"), nil
	}
	job, err := s.installation.Status(ctx, id)
	return installJobResult(job, err), nil
}

func (s *mcpServer) handleInstallResume(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.installation.Resume == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return errResult(errCodeUnsupported, "installation jobs require a host MCP session with the installation backend"), nil
	}
	opts := onboarding.ResumeOptions{
		JobID: strings.TrimSpace(stringParam(req, "job_id")), TargetID: stringParam(req, "target_id"),
		Address: stringParam(req, "address"), PublicKey: stringParam(req, "expected_public_key"),
	}
	if opts.JobID == "" {
		return errResult(errCodeInvalidArgument, "job_id is required"), nil
	}
	for name, dest := range map[string]*bool{"confirm_erase": &opts.ConfirmErase, "confirm_internal": &opts.ConfirmInternal} {
		if value, exists := req.GetArguments()[name]; exists {
			b, valid := value.(bool)
			if !valid {
				return errResultf(errCodeInvalidArgument, "%s must be a boolean", name), nil
			}
			*dest = b
		}
	}
	if opts.ConfirmErase && strings.TrimSpace(opts.TargetID) == "" {
		return errResult(errCodeInvalidArgument, "target_id from the latest probe is required with confirm_erase"), nil
	}
	job, err := s.installation.Resume(ctx, opts)
	return installJobResult(job, err), nil
}

func installJobResult(job *onboarding.Job, err error) *mcpgo.CallToolResult {
	if err == nil {
		return okResult(job)
	}
	code := errCodeInvalidArgument
	switch {
	case errors.Is(err, onboarding.ErrJobUnsupported):
		code = errCodeUnsupported
	case errors.Is(err, os.ErrNotExist):
		code = errCodeNotFound
	case errors.Is(err, onboarding.ErrArtifactUnavailable):
		code = errCodeInternal
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = errCodeTimeout
	}
	return errResult(code, err.Error())
}
