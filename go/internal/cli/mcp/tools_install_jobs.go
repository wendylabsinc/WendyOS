package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func (s *mcpServer) registerInstallationJobTools(srv *server.MCPServer) {
	start := append([]mcpgo.ToolOption{mcpgo.WithDescription("Create a persistent installation job and return physical setup instructions. Does not erase anything. Optional device_name and wifi are written for first boot; without them the image is written as published, so the board gets an auto-generated name and no Wi-Fi (Ethernet or USB). Resume after the user completes the step; only supported image installs can execute.")}, installationStartSchema()...)
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

// installationStartSchema adds optional first-boot inputs to the plan schema.
// Old relay catalogs stay valid: nothing is required and nothing has a default.
func installationStartSchema() []mcpgo.ToolOption {
	return append(installationOptionSchema(),
		mcpgo.WithString("device_name", mcpgo.Description("First-boot device name: 3-55 of a-z, 0-9 and -, starting with a letter. Omitted: auto-generated")),
		mcpgo.WithArray("wifi", mcpgo.Description("First-boot Wi-Fi networks. Omitted: no Wi-Fi. Hosted chats cannot send passwords"), mcpgo.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ssid":     map[string]any{"type": "string"},
				"password": map[string]any{"type": "string", "description": "Omit for an open network"},
				"priority": map[string]any{"type": "integer", "description": "Higher connects first"},
				"hidden":   map[string]any{"type": "boolean"},
				"security": map[string]any{"type": "string", "description": "For example wpa2, wpa3 or open; omitted detects it"},
			},
			"required":             []string{"ssid"},
			"additionalProperties": false,
		})),
		mcpgo.WithBoolean("no_wifi", mcpgo.Description("Explicitly no first-boot Wi-Fi; the same as omitting wifi")),
	)
}

func (s *mcpServer) handleInstallStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.installation.Start == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return errResult(errCodeUnsupported, "installation jobs require a host MCP session with the installation backend"), nil
	}
	if s.bindingPolicy() != nil {
		if _, refused := relayCredentialArgument("os_install_start", req.GetArguments()); refused {
			return errResult(errCodeDenied, relayWiFiPasswordRefusal), nil
		}
	}
	opts, err := installationOptions(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	prov, err := installationProvisioning(req.GetArguments())
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	job, err := s.installation.Start(ctx, onboarding.StartOptions{Options: opts, Provisioning: prov})
	return installJobResult(job, err), nil
}

const relayWiFiPasswordRefusal = "Wi-Fi passwords cannot be sent through a hosted chat. Run 'wendy install jobs start --wifi ssid=<name>,password=<password> ...' in a terminal on this host and continue with its job_id, or use a local MCP server (wendy mcp serve). device_name, no_wifi and open networks (networks that have no password) work here."

// relayCredentialArgument names an argument that carries a credential. A
// hosted chat must never collect one, so the relay refuses it before any prompt.
func relayCredentialArgument(tool string, args map[string]any) (string, bool) {
	if tool != "os_install_start" {
		return "", false
	}
	networks, _ := args["wifi"].([]any)
	for _, n := range networks {
		network, _ := n.(map[string]any)
		if v, ok := network["password"]; ok && v != nil && v != "" {
			return "wifi[].password", true
		}
	}
	return "", false
}

// installationProvisioning reads the optional first-boot arguments. Errors
// name fields, never values, because a password can be in any of them.
func installationProvisioning(args map[string]any) (*onboarding.Provisioning, error) {
	p := &onboarding.Provisioning{}
	if v, ok := args["device_name"]; ok && v != nil {
		name, valid := v.(string)
		if !valid {
			return nil, errors.New("device_name must be a string")
		}
		p.DeviceName = name
	}
	if v, ok := args["no_wifi"]; ok && v != nil {
		noWiFi, valid := v.(bool)
		if !valid {
			return nil, errors.New("no_wifi must be a boolean")
		}
		p.NoWiFi = noWiFi
	}
	if v, ok := args["wifi"]; ok && v != nil {
		networks, valid := v.([]any)
		if !valid {
			return nil, errors.New("wifi must be an array of networks")
		}
		for i, item := range networks {
			n, err := installationWiFiNetwork(item)
			if err != nil {
				return nil, fmt.Errorf("wifi[%d]: %w", i, err)
			}
			p.WiFi = append(p.WiFi, n)
		}
	}
	return p, nil
}

func installationWiFiNetwork(item any) (onboarding.WiFiNetwork, error) {
	var n onboarding.WiFiNetwork
	fields, ok := item.(map[string]any)
	if !ok {
		return n, errors.New("must be an object with ssid")
	}
	for key, v := range fields {
		switch key {
		case "ssid", "password", "security":
			text, ok := v.(string)
			if !ok {
				return n, fmt.Errorf("%s must be a string", key)
			}
			switch key {
			case "ssid":
				n.SSID = text
			case "password":
				n.Password = text
			default:
				n.Security = text
			}
		case "priority":
			f, ok := v.(float64)
			if !ok || f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
				return n, errors.New("priority must be an integer")
			}
			n.Priority = int32(f)
		case "hidden":
			hidden, ok := v.(bool)
			if !ok {
				return n, errors.New("hidden must be a boolean")
			}
			n.Hidden = hidden
		default:
			// A misspelled password field would otherwise seed an open network.
			return n, errors.New("unknown field; use ssid, password, priority, hidden or security")
		}
	}
	return n, nil
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
