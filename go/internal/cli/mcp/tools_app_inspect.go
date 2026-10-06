package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

func (s *mcpServer) registerAppInspectTools(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Inspect app and service states, recorded exit, resource usage, and recent warning/error logs. Optional project_path checks declared TCP readiness; missing or unsupported checks stay unknown."),
		mcpgo.WithString("app_name", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(256), mcpgo.Description("App ID from container_list")),
		mcpgo.WithString("project_path", mcpgo.Description("Local directory containing this app's wendy.json readiness declarations")),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(15), mcpgo.DefaultNumber(3), mcpgo.Description("Total inspection and probe deadline")),
		mcpgo.WithInteger("max_logs", mcpgo.Min(0), mcpgo.Max(200), mcpgo.DefaultNumber(20), mcpgo.Description("Recent warning/error records; zero skips logs")),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1024), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384), mcpgo.Description("JSON response budget; logs are dropped before app state")),
	}
	opts = append(opts, readOnly()...)
	opts = append(opts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("app_inspect", opts...), s.handleAppInspect)
}

type appInspectProbe struct {
	Service   string `json:"service,omitempty"`
	Port      int    `json:"port"`
	Container string `json:"-"`
}

func (s *mcpServer) handleAppInspect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	app := stringParam(req, "app_name")
	if len(app) > 256 || appconfig.ValidateAppID(app) != nil {
		return errResult(errCodeInvalidArgument, "app_name must be a valid app ID from container_list"), nil
	}
	timeout, err := ros2Int(req, "timeout_seconds", 3, 1, 15)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	maxLogs, err := ros2Int(req, "max_logs", 20, 0, 200)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	maxBytes, err := ros2Int(req, "max_bytes", 16384, 1024, 1000000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	project := stringParam(req, "project_path")
	if value, exists := req.GetArguments()["project_path"]; exists {
		if _, ok := value.(string); !ok {
			return errResult(errCodeInvalidArgument, "project_path must be a string"), nil
		}
	}
	probes, err := appInspectDeclarations(project, app)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn, transport, target := s.connectionSnapshot()
	if conn == nil {
		return errNotConnected(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{}, grpc.MaxCallRecvMsgSize(1024*1024))
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	var appContainer *agentpb.AppContainer
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return errResult(codeFromGRPC(recvErr), grpcErrString(recvErr)), nil
		}
		if resp.GetContainer().GetAppName() == app {
			appContainer = resp.GetContainer()
			break
		}
	}
	if appContainer == nil {
		return errResultf(errCodeNotFound, "app %q was not found on the connected device", app), nil
	}
	state, members := appInspectState(appContainer)
	var usage, logs, readiness map[string]any
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); usage = appInspectUsage(ctx, conn, members) }()
	go func() { defer wg.Done(); logs = appInspectLogs(ctx, conn, app, maxLogs, maxBytes/2) }()
	go func() {
		defer wg.Done()
		readiness = appInspectReadiness(ctx, conn, transport, target, project, probes, members, appInspectSimulatorAddress)
	}()
	wg.Wait()
	out := map[string]any{"app_name": app, "state": state, "usage": usage, "recent_logs": logs, "readiness": readiness}
	return appInspectBounded(out, maxBytes), nil
}

func appInspectState(c *agentpb.AppContainer) (map[string]any, map[string]bool) {
	out := map[string]any{"running_state": c.GetRunningState().String(), "version": c.GetAppVersion(), "failure_count": c.GetFailureCount()}
	exit := map[string]any{"status": "unknown"}
	if c.GetTerminationReason() != "" {
		exit = map[string]any{"status": "recorded", "reason": c.GetTerminationReason(), "code": c.GetExitCode()}
	}
	out["last_exit"] = exit
	if c.GetHttpPort() > 0 {
		out["declared_http_port"] = c.GetHttpPort()
	}
	members := map[string]bool{}
	services := []map[string]any{}
	allRunning := c.GetRunningState() == agentpb.AppRunningState_RUNNING
	for _, service := range c.GetServices() {
		name := c.GetAppName()
		if service.GetName() != "" {
			name += "_" + service.GetName()
		}
		members[name] = service.GetRunningState() == agentpb.AppRunningState_RUNNING
		services = append(services, map[string]any{"name": service.GetName(), "container_name": name, "running_state": service.GetRunningState().String()})
		allRunning = allRunning && service.GetRunningState() == agentpb.AppRunningState_RUNNING
	}
	if len(services) == 0 {
		members[c.GetAppName()] = c.GetRunningState() == agentpb.AppRunningState_RUNNING
	}
	out["services"], out["all_services_running"] = services, allRunning
	return out, members
}

func appInspectUsage(ctx context.Context, conn *grpcclient.AgentConnection, members map[string]bool) map[string]any {
	rows := map[string]map[string]any{}
	for name := range members {
		rows[name] = map[string]any{"container_name": name, "status": "unknown"}
	}
	warnings := []string{}
	measurement := func(row map[string]any, key string, value any, nonzero bool) {
		row[key] = value
		if !nonzero {
			row[key], row[key+"_status"] = nil, "unknown"
		} else {
			delete(row, key+"_status")
		}
	}
	stats, err := conn.ContainerService.ListContainerStats(ctx, &agentpb.ListContainerStatsRequest{}, grpc.MaxCallRecvMsgSize(1024*1024))
	if err != nil {
		warnings = append(warnings, "memory/storage: "+grpcErrString(err))
	} else {
		for _, item := range stats.GetStats() {
			if row := rows[item.GetAppName()]; row != nil {
				row["status"] = "reported"
				measurement(row, "memory_bytes", item.GetMemoryBytes(), item.GetMemoryBytes() > 0)
				measurement(row, "image_content_bytes", item.GetStorageBytes(), item.GetStorageBytes() > 0)
			}
		}
	}
	resources, err := conn.ContainerService.GetResourceStats(ctx, &agentpb.GetResourceStatsRequest{}, grpc.MaxCallRecvMsgSize(1024*1024))
	if err != nil {
		warnings = append(warnings, "CPU/memory: "+grpcErrString(err))
	} else {
		for _, item := range resources.GetContainers() {
			if row := rows[item.GetAppName()]; row != nil {
				row["status"] = "reported"
				// Keep an earlier positive memory reading when this RPC returns
				// its ambiguous zero fallback.
				if item.GetMemoryBytes() > 0 || row["memory_bytes"] == nil {
					measurement(row, "memory_bytes", item.GetMemoryBytes(), item.GetMemoryBytes() > 0)
				}
				measurement(row, "cpu_usage_nanos", item.GetCpuUsageNanos(), item.GetCpuUsageNanos() > 0)
			}
		}
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(rows))
	for _, name := range names {
		items = append(items, rows[name])
	}
	return map[string]any{"containers": items, "warnings": warnings, "note": "Zero counters are unknown because the agent also uses zero when measurements are unavailable; CPU is cumulative nanoseconds."}
}

func appInspectLogs(ctx context.Context, conn *grpcclient.AgentConnection, app string, maxRows, maxBytes int) map[string]any {
	rows := []map[string]any{}
	out := map[string]any{"status": "unknown", "records": rows, "min_severity": 13, "collection_limited": false}
	if maxRows == 0 {
		out["status"] = "not_requested"
		return out
	}
	if conn.TelemetryService == nil {
		out["reason"] = "telemetry service unavailable"
		return out
	}
	logCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lastN, severity := int32(20), int32(13)
	stream, err := conn.TelemetryService.StreamLogs(logCtx, &agentpb.StreamLogsRequest{AppName: &app, LastN: &lastN, MinSeverity: &severity}, grpc.MaxCallRecvMsgSize(1024*1024))
	if err != nil {
		out["reason"] = grpcErrString(err)
		return out
	}
	collected, batches, omitted := 0, 0, 0
	for batches < 20 {
		response, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr != io.EOF && ctx.Err() == nil {
				out["reason"] = grpcErrString(recvErr)
			}
			if ctx.Err() != nil {
				out["collection_limited"] = true
			}
			break
		}
		if response.GetLogs() == nil {
			continue
		}
		batches++
		data, marshalErr := telemetryProtoJSON.Marshal(response)
		if marshalErr != nil {
			out["reason"] = marshalErr.Error()
			break
		}
		events, decodeErr := compactTelemetryRows("logs", []json.RawMessage{data})
		if decodeErr != nil {
			out["reason"] = decodeErr.Error()
			break
		}
		collected += len(events)
		for _, event := range events {
			event["is_history"] = response.GetIsHistory()
			rows = append(rows, event)
		}
		for len(rows) > maxRows {
			rows = rows[1:]
			omitted++
		}
		for len(rows) > 0 {
			encoded, _ := json.Marshal(rows)
			if len(encoded) <= maxBytes {
				break
			}
			rows = rows[1:]
			omitted++
		}
	}
	if batches >= 20 {
		out["collection_limited"] = true
	}
	if collected > 0 {
		out["status"] = "observed"
	} else if _, failed := out["reason"]; !failed {
		out["reason"] = "no warning/error records observed in this bounded collection"
	}
	out["records"], out["collected"], out["omitted"], out["batches_collected"] = rows, collected, omitted, batches
	return out
}

func appInspectDeclarations(project, app string) ([]appInspectProbe, error) {
	if project == "" {
		return nil, nil
	}
	path := filepath.Join(project, "wendy.json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("project_path must contain a regular wendy.json file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading project readiness: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, fmt.Errorf("wendy.json must be readable and at most 1 MiB")
	}
	cfg, err := appconfig.LoadFromBytes(data)
	if err != nil {
		return nil, err
	}
	if cfg.AppID != app {
		return nil, fmt.Errorf("project appId %q does not match app_name %q", cfg.AppID, app)
	}
	probes := []appInspectProbe{}
	add := func(service string, readiness *appconfig.ReadinessConfig) error {
		if err := appconfig.ValidateReadiness("readiness", readiness); err != nil {
			return err
		}
		if readiness != nil && readiness.TCPSocket != nil {
			if len(probes) >= 64 {
				return fmt.Errorf("project has more than 64 readiness probes; inspect a smaller project")
			}
			probes = append(probes, appInspectProbe{Service: service, Port: readiness.TCPSocket.Port, Container: app + "_" + service})
		}
		return nil
	}
	if err := add("", cfg.Readiness); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := appconfig.ValidateServiceName(name); err != nil {
			return nil, err
		}
		if cfg.Services[name] == nil {
			return nil, fmt.Errorf("services[%q] must not be null", name)
		}
		if err := add(name, cfg.Services[name].Readiness); err != nil {
			return nil, err
		}
	}
	return probes, nil
}

type appInspectSimulatorResolver func(context.Context, *grpcclient.AgentConnection, int) (string, error)

func appInspectProbeAddress(ctx context.Context, conn *grpcclient.AgentConnection, transport string, target commandTarget, port int, simulator appInspectSimulatorResolver) (string, error) {
	if transport == "cloud" || target.Transport == "cloud" || conn.RegistryDialer != nil {
		return "", fmt.Errorf("cloud tunnel creation does not confirm remote TCP readiness")
	}
	if conn.SimulatorName != "" {
		return simulator(ctx, conn, port)
	}
	if conn.IsSessionProxy || strings.HasPrefix(conn.Host, "unix:") {
		return "", fmt.Errorf("this proxy connection has no verified application TCP route")
	}
	host, _, err := net.SplitHostPort(conn.Addr)
	if err != nil || host == "" {
		return "", fmt.Errorf("connection has no verified direct TCP address")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func appInspectSimulatorAddress(ctx context.Context, conn *grpcclient.AgentConnection, port int) (string, error) {
	store, err := vm.NewStore()
	if err != nil {
		return "", err
	}
	st, err := store.Status(conn.SimulatorName)
	if err != nil {
		return "", err
	}
	host, portText, err := net.SplitHostPort(conn.Addr)
	if err != nil {
		return "", err
	}
	agentPort, err := strconv.Atoi(portText)
	if err != nil || host != "127.0.0.1" || !st.Running || (agentPort != st.State.AgentPort && agentPort != st.State.AgentPort+1) {
		return "", fmt.Errorf("simulator no longer owns the connected agent endpoint")
	}
	forwarded, err := store.TCPPortMapping(ctx, conn.SimulatorName, port)
	if err != nil {
		return "", err
	}
	if forwarded == 0 {
		return "", fmt.Errorf("simulator has no existing forward for declared TCP port %d", port)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(forwarded)), nil
}

func appInspectReadiness(ctx context.Context, conn *grpcclient.AgentConnection, transport string, target commandTarget, project string, probes []appInspectProbe, members map[string]bool, simulator appInspectSimulatorResolver) map[string]any {
	out := map[string]any{"status": "unknown", "checks": []map[string]any{}}
	if project == "" {
		out["reason"] = "project_path is needed to read declared readiness; the agent does not report stored probe results"
		return out
	}
	out["configuration_source"], out["deployed_configuration_verified"] = "local_project", false
	if len(probes) == 0 {
		out["reason"] = "project declares no TCP readiness probes"
		return out
	}
	checks := make([]map[string]any, len(probes))
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Add(1)
		go func(i int, probe appInspectProbe) {
			defer wg.Done()
			check := map[string]any{"kind": "tcp_socket", "port": probe.Port, "status": "unknown"}
			checks[i] = check
			if probe.Service != "" {
				check["service"] = probe.Service
				running, present := members[probe.Container]
				if !present {
					check["reason"] = "declared service is absent from the device app"
					return
				}
				if !running {
					check["status"], check["reason"] = "failed", "declared service is not running"
					return
				}
			} else {
				if len(members) == 0 {
					check["reason"] = "app running state is unavailable"
					return
				}
				for _, running := range members {
					if !running {
						check["status"], check["reason"] = "failed", "app readiness requires every service to be running"
						return
					}
				}
			}
			addr, err := appInspectProbeAddress(ctx, conn, transport, target, probe.Port, simulator)
			if err != nil {
				check["reason"] = err.Error()
				return
			}
			if conn.SimulatorName != "" {
				check["forward_address"] = addr
				check["reason"] = "verified simulator forward; a local TCP accept does not prove the guest application is ready"
				return
			}
			dialer := net.Dialer{Timeout: 2 * time.Second}
			connection, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				check["status"], check["reason"] = "failed", "TCP connection could not be established: "+err.Error()
				return
			}
			_ = connection.Close()
			check["status"] = "passed"
		}(i, probe)
	}
	wg.Wait()
	result := "passed"
	for _, check := range checks {
		if check["status"] == "failed" {
			result = "failed"
			break
		}
		if check["status"] == "unknown" {
			result = "unknown"
		}
	}
	out["status"], out["checks"], out["scope"] = result, checks, "declared TCP connectivity only"
	return out
}

// Preserve state even when remote diagnostics or a large service group exceed
// the budget. Logs are the first information discarded, followed by detail.
func appInspectBounded(out map[string]any, maxBytes int) *mcpgo.CallToolResult {
	fits := func() bool { data, _ := json.Marshal(out); return len(data) <= maxBytes }
	if fits() {
		return okResult(out)
	}
	out["truncated"] = true
	logs := out["recent_logs"].(map[string]any)
	rows := logs["records"].([]map[string]any)
	for len(rows) > 0 && !fits() {
		rows = rows[1:]
		logs["records"] = rows
		omitted, _ := logs["omitted"].(int)
		logs["omitted"] = omitted + 1
	}
	if fits() {
		return okResult(out)
	}
	state := out["state"].(map[string]any)
	minimal := map[string]any{"app_name": out["app_name"], "running_state": state["running_state"], "all_services_running": state["all_services_running"], "failure_count": state["failure_count"], "truncated": true, "readiness": map[string]any{"status": "unknown", "reason": "details exceed max_bytes"}, "note": "Increase max_bytes for service, exit, usage, log, and readiness details."}
	if exit, ok := state["last_exit"].(map[string]any); ok {
		lastExit := map[string]any{"status": exit["status"]}
		if code, exists := exit["code"]; exists {
			lastExit["code"] = code
		}
		if reason, ok := exit["reason"].(string); ok {
			if len(reason) > 128 {
				reason = strings.ToValidUTF8(reason[:128], "")
			}
			lastExit["reason"] = reason
		}
		minimal["last_exit"] = lastExit
	}
	return okResult(minimal)
}
