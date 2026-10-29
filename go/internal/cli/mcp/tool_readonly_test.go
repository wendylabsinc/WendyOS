package mcp

import (
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// readOnlyTools is every built-in tool marked read-only. wendy chat runs these
// without asking the user, including in turns a watch event starts by itself,
// so a tool belongs here only if calling it changes nothing on the device, in
// Cloud or on this computer. Adding a name is a review decision: read the
// tool's handler before you do.
//
// On the device the rule also holds by itself in those turns: chat marks their
// calls read-only, and the server then refuses every agent RPC that grpcclient
// does not classify as a read (readonly_call.go). Cloud and this computer
// still rest on this list.
//
// Two listed tools have effects that are not changes to the user's things:
// wendy_tools changes which tools this session advertises (in memory only), and
// the ros2_* tools' host scope may pull an inspector image onto the device.
// job_inspect and compute_status run a fixed read-only script through
// HostShell, a write RPC to the device guard, so they fail in those turns.
//
// The list covers built-in tools only. A tool a device app provides
// (<app>__<tool>) carries the app's own hint, which nothing here vouches for,
// so chat refuses those in turns a watch event starts.
var readOnlyTools = []string{
	"app_inspect", "bluetooth_scan", "camera_controls", "camera_list", "cloud_discover", "cloud_ping",
	"cloud_tunnel_list", "compute_status", "container_list", "container_stats", "device_info", "device_list",
	"device_os_logs", "hardware_capabilities", "job_inspect", "os_install_plan", "os_install_status",
	"os_install_verify", "os_list_drives",
	"os_update_status", "project_validate", "provisioning_status", "ros2_lidar_summary", "ros2_topic_hz",
	"ros2_topic_info", "ros2_topic_sample", "ros2_topics", "simulator_list", "telemetry_logs", "telemetry_metrics",
	"telemetry_traces", "watch_events", "watch_list", "watch_sources", "wendy_status", "wendy_tools",
	"wifi_known_networks", "wifi_list", "wifi_status",
}

func TestReadOnlyToolsAreAnExplicitList(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	tools := srv.ListTools()
	for name, tool := range tools {
		hint := tool.Tool.Annotations.ReadOnlyHint
		marked := hint != nil && *hint
		listed := slices.Contains(readOnlyTools, name)
		switch {
		case marked && !listed:
			t.Errorf("%s is marked read-only but not in readOnlyTools; chat would run it without approval. Add it only after checking it changes nothing.", name)
		case listed && !marked:
			t.Errorf("%s is in readOnlyTools but not marked read-only; remove it from the list", name)
		}
	}
	for _, name := range readOnlyTools {
		if _, ok := tools[name]; !ok {
			t.Errorf("readOnlyTools names %s, which is not a tool", name)
		}
	}
}

// Chat tells a device app's tool from a built-in one by the "__" in its name
// (<app>__<tool>, see container_mcp.go) and refuses app tools in turns a watch
// report started, whatever read-only hint the app sets. A built-in name with
// that separator would be refused too, so none may have one.
func TestNoBuiltInToolNameContainsTheAppSeparator(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	for name := range srv.ListTools() {
		if strings.Contains(name, "__") {
			t.Errorf("built-in tool %q contains \"__\", the separator of app tool names", name)
		}
	}
}
