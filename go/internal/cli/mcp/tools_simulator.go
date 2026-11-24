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
	Start       func(context.Context, string) (*SimulatorInfo, error)
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
	Arch      string `json:"arch,omitempty"`
	Version   string `json:"version,omitempty"`
	Source    string `json:"source,omitempty"`
	DiskBytes int64  `json:"disk_bytes,omitempty"`
	Address   string `json:"address,omitempty"`
	Error     string `json:"error,omitempty"`
}

type SimulatorCreateOptions struct {
	// Version is the WendyOS version to download, or for a Wendy Lite
	// profile the Wendy Lite version, branch or commit to build.
	Name, Profile, Image, Version string
	// Nightly builds the Wendy Lite nightly tag; only the CLI sets it.
	Nightly bool
	// Arch is the guest architecture, arm64 or amd64; empty uses the host's
	// default, and ARM64 for a local image.
	Arch    string
	DiskGiB int
}

var simulatorVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// liteSimulatorVersionPattern also accepts the slashes of a branch name.
var liteSimulatorVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,127}$`)

func (o SimulatorCreateOptions) Validate() error {
	if err := vm.ValidName(o.Name); err != nil {
		return err
	}
	if !vm.IsLiteProfile(o.Profile) && o.Profile != "generic" && o.Profile != "go2" && o.Profile != "g1" && o.Profile != "rosmaster-r2" && o.Profile != "so101" {
		return fmt.Errorf("profile must be generic, go2, g1, so101, rosmaster-r2, wendy-lite or wendy-lite-native")
	}
	if o.DiskGiB < 1 || o.DiskGiB > 1024 {
		return fmt.Errorf("disk_gib must be an integer in 1..1024")
	}
	if o.Image != "" && o.Version != "" {
		return fmt.Errorf("image and version cannot be combined")
	}
	if o.Nightly && (o.Image != "" || o.Version != "") {
		return fmt.Errorf("nightly cannot be combined with image or version")
	}
	if o.Nightly && !vm.IsLiteProfile(o.Profile) {
		return fmt.Errorf("nightly applies to wendy-lite and wendy-lite-native")
	}
	if len(o.Image) > 4096 {
		return fmt.Errorf("image path exceeds 4096 bytes")
	}
	if o.Arch != "" && o.Arch != "arm64" && o.Arch != "amd64" {
		return fmt.Errorf("arch must be arm64 or amd64")
	}
	if o.Arch != "" && vm.IsLiteProfile(o.Profile) {
		return fmt.Errorf("arch applies to WendyOS simulators; %s runs ESP32-C6 firmware", o.Profile)
	}
	if o.Version != "" && vm.IsLiteProfile(o.Profile) {
		if !liteSimulatorVersionPattern.MatchString(o.Version) {
			return fmt.Errorf("version must be a Wendy Lite version, branch or commit of at most 128 letters, digits, dots, dashes, underscores, plus signs or slashes")
		}
	} else if o.Version != "" && !simulatorVersionPattern.MatchString(o.Version) {
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
		mcpgo.WithDescription("Create a stopped local simulator from a published or local image. Downloads may take several minutes. Use simulator_start to boot and verify it. wendy-lite runs WASM firmware; wendy-lite-native runs native ESP-IDF firmware with OTA slots. Both use ESP32-C6 with esp-emu. Without image, they build Wendy Lite on this computer: this needs EIM and git, and a first build installs ESP-IDF and takes many minutes. image must be a merged flash binary."),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("1–32 lowercase letters, digits or dashes; starts and ends with a letter or digit")),
		mcpgo.WithString("profile", mcpgo.Enum("generic", "go2", "g1", "so101", "rosmaster-r2", "wendy-lite", "wendy-lite-native"), mcpgo.DefaultString("generic")),
		mcpgo.WithString("image", mcpgo.Description("Local raw, ZIP, gzip or zstd image path; mutually exclusive with version")),
		mcpgo.WithString("version", mcpgo.Description("Published version; omitted downloads latest stable. For wendy-lite and wendy-lite-native: the Wendy Lite version, branch or commit to build; omitted builds the latest release")),
		mcpgo.WithString("arch", mcpgo.Enum("arm64", "amd64"), mcpgo.Description("Guest architecture; omitted uses this computer's default (a local image is treated as arm64)")),
		mcpgo.WithInteger("disk_gib", mcpgo.Min(1), mcpgo.Max(1024), mcpgo.DefaultNumber(16)),
	}
	create = append(create, mutating()...)
	create = append(create, openWorld()...)
	srv.AddTool(mcpgo.NewTool("simulator_create", create...), s.handleSimulatorCreate)
	start := []mcpgo.ToolOption{mcpgo.WithDescription("Start a local simulator and verify its device protocol. Wendy Lite uses real ESP32-C6 firmware and requires esp-emu on PATH or installed by the interactive Wendy CLI."), mcpgo.WithString("name", mcpgo.Required())}
	start = append(start, mutating()...)
	start = append(start, localOnly()...)
	srv.AddTool(mcpgo.NewTool("simulator_start", start...), s.handleSimulatorStart)
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
	}{{"name", "", &opts.Name}, {"profile", "generic", &opts.Profile}, {"image", "", &opts.Image}, {"version", "", &opts.Version}, {"arch", "", &opts.Arch}} {
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
	next := "device_connect"
	if vm.IsLiteProfile(opts.Profile) {
		next = "simulator_start"
	}
	return okResult(map[string]any{"simulator": info, "readiness": "not_checked", "next_step": next, "device": "vm:" + opts.Name}), nil
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

func (s *mcpServer) handleSimulatorStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.simulators.Start == nil || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return simulatorUnavailable(), nil
	}
	name, err := simulatorNameParam(req)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	info, err := s.simulators.Start(ctx, name)
	if err != nil {
		return simulatorFailure(err), nil
	}
	return okResult(map[string]any{"simulator": info, "device": info.Device, "readiness": "verified"}), nil
}
