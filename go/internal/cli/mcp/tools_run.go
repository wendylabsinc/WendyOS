package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Explicit selectors override the session. Never let a child choose a different
// default device, or lose the cloud organization and broker of a live session.
func (s *mcpServer) runTarget(req mcpgo.CallToolRequest) (commandTarget, error) {
	device, cloud := stringParam(req, "device"), stringParam(req, "device_name")
	endpoint, broker := stringParam(req, "cloud_grpc"), stringParam(req, "broker_url")
	for _, value := range []string{device, cloud, endpoint, broker} {
		if value != "" && strings.TrimSpace(value) == "" {
			return commandTarget{}, fmt.Errorf("device and cloud selectors cannot be whitespace")
		}
	}
	if device != "" {
		if cloud != "" || endpoint != "" || broker != "" {
			return commandTarget{}, fmt.Errorf("device cannot be combined with device_name, cloud_grpc, or broker_url")
		}
		return commandTarget{Device: device, Transport: "selector"}, nil
	}
	if cloud != "" {
		return commandTarget{Device: cloud, Transport: "cloud", CloudGRPC: endpoint, BrokerURL: broker}, nil
	}
	_, _, target := s.connectionSnapshot()
	if target.Device == "" {
		return target, fmt.Errorf("pass device or device_name, or connect to a replayable device target first")
	}
	if endpoint != "" || broker != "" {
		return commandTarget{}, fmt.Errorf("cloud endpoint overrides require an explicit device_name; omit them to reuse the connected target")
	}
	return target, nil
}

func (s *mcpServer) handleRun(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return errResult(errCodeUnsupported, "run from a host MCP session without WENDY_AGENT_SOCKET overriding the selected deployment target"), nil
	}
	projectPath := stringParam(req, "project_path")
	if projectPath == "" {
		return errResult(errCodeInvalidArgument, "project_path is required"), nil
	}
	projectPath, err := filepath.Abs(projectPath)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	if info, err := os.Stat(filepath.Join(projectPath, "wendy.json")); err != nil || info.IsDir() {
		return errResult(errCodeInvalidArgument, "project_path must contain a wendy.json file"), nil
	}
	target, err := s.runTarget(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	timeout, err := ros2Int(req, "timeout_seconds", 300, 1, 3600)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	maxBytes, err := ros2Int(req, "max_bytes", 16384, 1, 1000000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	start := true
	if value, present := req.GetArguments()["start"]; present {
		var ok bool
		start, ok = value.(bool)
		if !ok {
			return errResult(errCodeInvalidArgument, "start must be a boolean"), nil
		}
		if legacy, present := req.GetArguments()["deploy"]; present && legacy != !start {
			return errResult(errCodeInvalidArgument, "start conflicts with legacy deploy"), nil
		}
	} else {
		start = !req.GetBool("deploy", false)
	}
	selector := target.Device
	if target.Selector != "" {
		selector = target.Selector
	}
	args := []string{"run", "--prefix", projectPath, "--device", selector, "--yes"}
	if target.Transport == "cloud" && target.Selector == "" {
		args = append([]string{"cloud"}, args...)
		if target.CloudGRPC != "" {
			args = append(args, "--cloud-grpc", target.CloudGRPC)
		}
		if target.BrokerURL != "" {
			args = append(args, "--broker-url", target.BrokerURL)
		}
	}
	for _, name := range []string{"build_type", "product"} {
		if value := stringParam(req, name); value != "" {
			args = append(args, "--"+strings.ReplaceAll(name, "_", "-"), value)
		}
	}
	if !start {
		args = append(args, "--deploy")
	}
	for _, name := range []string{"debug", "detach"} {
		if req.GetBool(name, name == "detach") {
			args = append(args, "--"+name)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	runner := s.runCommandFn
	if runner == nil {
		runner = executeRunCommand
	}
	tok := progressToken(req)
	reportProgress(ctx, tok, 0, 0, "building and deploying to "+target.Device)
	output, truncated, runErr := runner(runCtx, args, target, maxBytes)
	s.refreshContainerMCPTools()
	result := map[string]any{
		"target": target, "output": strings.TrimSpace(output), "truncated": truncated,
		"readiness": "not_checked",
	}
	if runErr != nil || runCtx.Err() != nil {
		code := errCodeInternal
		if runCtx.Err() != nil {
			code = errCodeTimeout
		}
		result["error_code"] = string(code)
		result["status"] = "failed"
		result["message"] = "Deployment did not complete. Inspect the output and device state before retrying; a timed-out command may already have created or started the container."
		if output == "" && runErr != nil {
			result["output"] = runErr.Error()
		}
		r := okResult(result)
		r.IsError = true
		return r, nil
	}
	result["status"] = "started"
	if !start {
		result["status"] = "created"
	}
	result["suggested_next_step"] = "Connect to the returned target, check container_list and telemetry_logs, then test the app's health endpoint or ROS interface. Deployment alone does not verify behavior."
	reportProgress(ctx, tok, 1, 1, "deployment command completed")
	return okResult(result), nil
}

// Keep the tail while the process runs, including on failure. A large compiler
// log must not exhaust memory or hide the final diagnostic behind its prologue.
type runTail struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (w *runTail) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if len(w.data)+n > w.limit {
		w.truncated = true
		if n >= w.limit {
			w.data = append(w.data[:0], p[n-w.limit:]...)
		} else {
			w.data = append(w.data[len(w.data)+n-w.limit:], p...)
		}
	} else {
		w.data = append(w.data, p...)
	}
	return n, nil
}

func executeRunCommand(ctx context.Context, args []string, target commandTarget, limit int) (string, bool, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if target.Selector != "" && target.Transport == "cloud" {
		cmd.Env = runEnvironment(os.Environ(), target)
	}
	// Bound pipe cleanup if a descendant build process outlives the CLI.
	cmd.WaitDelay = 2 * time.Second
	tail := &runTail{limit: limit}
	cmd.Stdout, cmd.Stderr = tail, tail
	err = cmd.Run()
	data := tail.data
	for len(data) > 0 && !utf8.RuneStart(data[0]) {
		data = data[1:]
	}
	return strings.ToValidUTF8(string(data), "�"), tail.truncated, err
}

func runEnvironment(env []string, target commandTarget) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), "WENDY_BROKER_URL=") {
			out = append(out, entry)
		}
	}
	return append(out, "WENDY_BROKER_URL="+target.BrokerURL)
}
