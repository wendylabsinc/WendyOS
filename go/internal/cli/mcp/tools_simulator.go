package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// SimulatorBackend keeps host image downloads and VM provisioning in the CLI.
type SimulatorBackend struct {
	List        func(context.Context) ([]SimulatorInfo, error)
	Create      func(context.Context, SimulatorCreateOptions) (*SimulatorInfo, error)
	Stop        func(context.Context, string, bool, time.Duration) (*SimulatorInfo, error)
	Delete      func(context.Context, string) error
	Viewer      func(context.Context, string) (*SimulatorViewer, error)
	UpdateAgent func(context.Context, string) (*SimulatorAgentUpdate, error)
}

type SimulatorAgentUpdate struct {
	Name               string `json:"name"`
	Device             string `json:"device"`
	Version            string `json:"version"`
	Updated            bool   `json:"updated"`
	CapabilityVerified bool   `json:"capability_verified"`
}

type SimulatorViewer struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	URL     string `json:"url"`
	Ready   bool   `json:"ready"`
	Healthy bool   `json:"healthy"`
	Mode    string `json:"mode"`
}

type SimulatorInfo struct {
	Name      string `json:"name"`
	Device    string `json:"device"`
	State     string `json:"state"`
	Profile   string `json:"profile,omitempty"`
	Version   string `json:"version,omitempty"`
	Source    string `json:"source,omitempty"`
	DiskBytes int64  `json:"disk_bytes,omitempty"`
	Address   string `json:"address,omitempty"`
	Error     string `json:"error,omitempty"`
}

type SimulatorCreateOptions struct {
	Name, Profile, Image, Version string
	DiskGiB                       int
}

var simulatorVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

func (o SimulatorCreateOptions) Validate() error {
	if err := vm.ValidName(o.Name); err != nil {
		return err
	}
	if o.Profile != "generic" && o.Profile != "go2" && o.Profile != "g1" && o.Profile != "rosmaster-r2" {
		return fmt.Errorf("profile must be generic, go2, g1 or rosmaster-r2")
	}
	if o.DiskGiB < 1 || o.DiskGiB > 1024 {
		return fmt.Errorf("disk_gib must be an integer in 1..1024")
	}
	if o.Image != "" && o.Version != "" {
		return fmt.Errorf("image and version cannot be combined")
	}
	if len(o.Image) > 4096 {
		return fmt.Errorf("image path exceeds 4096 bytes")
	}
	if o.Version != "" && !simulatorVersionPattern.MatchString(o.Version) {
		return fmt.Errorf("version must be a release tag of at most 128 letters, digits, dots, dashes, underscores or plus signs")
	}
	return nil
}

// SetSimulatorBackend must be called before Start.
func (s *mcpServer) SetSimulatorBackend(backend SimulatorBackend) { s.simulators = backend }

func (s *mcpServer) registerSimulatorTools(srv *server.MCPServer) {
	list := []mcpgo.ToolOption{
		mcpgo.WithDescription("List local simulators and their vm:name device selectors. Running state does not establish agent or robot readiness."),
		mcpgo.WithInteger("max_results", mcpgo.Min(1), mcpgo.Max(1000), mcpgo.DefaultNumber(100)),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1024), mcpgo.Max(100000), mcpgo.DefaultNumber(16384)),
	}
	list = append(list, readOnly()...)
	list = append(list, localOnly()...)
	srv.AddTool(mcpgo.NewTool("simulator_list", list...), s.handleSimulatorList)
	create := []mcpgo.ToolOption{
		mcpgo.WithDescription("Create a stopped local simulator from a published or local image. Downloads may take several minutes. Connect to its vm:name selector to boot it and provision its robot profile."),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("1–32 lowercase letters, digits or dashes; starts and ends with a letter or digit")),
		mcpgo.WithString("profile", mcpgo.Enum("generic", "go2", "g1", "rosmaster-r2"), mcpgo.DefaultString("generic")),
		mcpgo.WithString("image", mcpgo.Description("Local raw, ZIP, gzip or zstd image path; mutually exclusive with version")),
		mcpgo.WithString("version", mcpgo.Description("Published version; omitted downloads latest stable")),
		mcpgo.WithInteger("disk_gib", mcpgo.Min(1), mcpgo.Max(1024), mcpgo.DefaultNumber(16)),
	}
	create = append(create, mutating()...)
	create = append(create, openWorld()...)
	srv.AddTool(mcpgo.NewTool("simulator_create", create...), s.handleSimulatorCreate)
	stop := []mcpgo.ToolOption{
		mcpgo.WithDescription("Shut down a local simulator. Timeout leaves it running; force explicitly cuts power and can lose guest data."),
		mcpgo.WithString("name", mcpgo.Required()),
		mcpgo.WithBoolean("force", mcpgo.DefaultBool(false)),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(120), mcpgo.DefaultNumber(60)),
	}
	stop = append(stop, destructive()...)
	stop = append(stop, idempotent()...)
	stop = append(stop, localOnly()...)
	srv.AddTool(mcpgo.NewTool("simulator_stop", stop...), s.handleSimulatorStop)
	del := []mcpgo.ToolOption{
		mcpgo.WithDescription("Permanently delete a stopped local simulator, including its disk and robot profile. Refuses running simulators; stop explicitly first."),
		mcpgo.WithString("name", mcpgo.Required()),
	}
	del = append(del, destructive()...)
	del = append(del, localOnly()...)
	srv.AddTool(mcpgo.NewTool("simulator_delete", del...), s.handleSimulatorDelete)
}

func simulatorString(req mcpgo.CallToolRequest, key, fallback string) (string, error) {
	v, exists := req.GetArguments()[key]
	if !exists {
		return fallback, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

func simulatorNameParam(req mcpgo.CallToolRequest) (string, error) {
	name, err := simulatorString(req, "name", "")
	if err != nil {
		return "", err
	}
	return name, vm.ValidName(name)
}

func simulatorFailure(err error) *mcpgo.CallToolResult {
	code := errCodeInternal
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = errCodeTimeout
	} else if errors.Is(err, os.ErrNotExist) {
		code = errCodeNotFound
	}
	return errResult(code, err.Error())
}

func simulatorUnavailable() *mcpgo.CallToolResult {
	return errResult(errCodeUnsupported, "local simulator management requires a host MCP server")
}

func (s *mcpServer) handleSimulatorList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.simulators.List == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return simulatorUnavailable(), nil
	}
	count, err := ros2Int(req, "max_results", 100, 1, 1000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	budget, err := ros2Int(req, "max_bytes", 16384, 1024, 100000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	items, err := s.simulators.List(ctx)
	if err != nil {
		return simulatorFailure(err), nil
	}
	return okRowsBounded("simulators", items, nil, budget, count), nil
}

func (s *mcpServer) handleSimulatorCreate(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.simulators.Create == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return simulatorUnavailable(), nil
	}
	var opts SimulatorCreateOptions
	for _, p := range []struct {
		key, fallback string
		out           *string
	}{{"name", "", &opts.Name}, {"profile", "generic", &opts.Profile}, {"image", "", &opts.Image}, {"version", "", &opts.Version}} {
		v, err := simulatorString(req, p.key, p.fallback)
		if err != nil {
			return errResult(errCodeInvalidArgument, err.Error()), nil
		}
		*p.out = v
	}
	var err error
	opts.DiskGiB, err = ros2Int(req, "disk_gib", 16, 1, 1024)
	if err == nil {
		err = opts.Validate()
	}
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	info, err := s.simulators.Create(ctx, opts)
	if err != nil {
		return simulatorFailure(err), nil
	}
	return okResult(map[string]any{"simulator": info, "readiness": "not_checked", "next_step": "device_connect", "device": "vm:" + opts.Name}), nil
}

func (s *mcpServer) handleSimulatorStop(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.simulators.Stop == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return simulatorUnavailable(), nil
	}
	name, err := simulatorNameParam(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	force := false
	if v, exists := req.GetArguments()["force"]; exists {
		var ok bool
		if force, ok = v.(bool); !ok {
			return errResult(errCodeInvalidArgument, "force must be a boolean"), nil
		}
	}
	seconds, err := ros2Int(req, "timeout_seconds", 60, 1, 120)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn, _, target := s.connectionSnapshot()
	info, err := s.simulators.Stop(ctx, name, force, time.Duration(seconds)*time.Second)
	if err != nil {
		return simulatorFailure(err), nil
	}
	disconnected := s.clearSimulatorConnection(name, conn, target)
	return okResult(map[string]any{"simulator": info, "disconnected": disconnected}), nil
}

func (s *mcpServer) handleSimulatorDelete(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.simulators.Delete == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return simulatorUnavailable(), nil
	}
	name, err := simulatorNameParam(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn, _, target := s.connectionSnapshot()
	if err := s.simulators.Delete(ctx, name); err != nil {
		return simulatorFailure(err), nil
	}
	disconnected := s.clearSimulatorConnection(name, conn, target)
	return okResult(map[string]any{"deleted": name, "disconnected": disconnected}), nil
}

// A completed stop/delete invalidates this VM's session, but must not clear a
// connection that another request selected while the shutdown was pending.
func (s *mcpServer) clearSimulatorConnection(name string, expected *grpcclient.AgentConnection, target commandTarget) bool {
	if expected == nil || (expected.SimulatorName != name && target.Device != "vm:"+name) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != expected {
		return false
	}
	s.setConnectionLocked(nil, "", commandTarget{})
	return true
}
