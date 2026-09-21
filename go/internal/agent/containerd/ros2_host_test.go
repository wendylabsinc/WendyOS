package containerd

import (
	"encoding/xml"
	"reflect"
	"strings"
	"testing"
)

func TestHostROS2InspectorHasOnlyExplicitHostNetworkAccess(t *testing.T) {
	spec := ros2HostSidecarSpec("/fixed/config.xml")
	if !spec.Root.Readonly || !spec.Process.NoNewPrivileges || spec.Process.User.UID == 0 {
		t.Fatal("host inspector must be unprivileged with read-only root")
	}
	if len(spec.Process.Capabilities.Bounding)+len(spec.Process.Capabilities.Permitted)+len(spec.Process.Capabilities.Effective) != 0 {
		t.Fatal("host inspector has capabilities")
	}
	private := map[string]bool{}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" || ns.Path != "" {
			t.Fatalf("unexpected namespace join: %+v", ns)
		}
		private[ns.Type] = true
	}
	for _, name := range []string{"ipc", "pid", "mount", "uts"} {
		if !private[name] {
			t.Errorf("missing private %s namespace", name)
		}
	}
	for _, mount := range spec.Mounts {
		if mount.Type == "bind" && (mount.Source != "/fixed/config.xml" || !strings.Contains(strings.Join(mount.Options, ","), "ro")) {
			t.Fatalf("unexpected host bind mount: %+v", mount)
		}
		if mount.Destination == "/dev/shm" && mount.Type != "tmpfs" {
			t.Fatal("host inspector shares host/app sample buffers")
		}
	}
	if !envContains(spec.Process.Env, "ROS_LOCALHOST_ONLY=0") || !envContains(spec.Process.Env, "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET") || !envContains(spec.Process.Env, "RMW_IMPLEMENTATION=rmw_fastrtps_cpp") {
		t.Fatalf("host DDS environment: %v", spec.Process.Env)
	}
}

func TestHostROS2InspectorDisablesSharedMemoryTransport(t *testing.T) {
	var profile struct {
		Descriptor struct {
			Transport struct {
				Type string `xml:"type"`
			} `xml:"transport_descriptor"`
		} `xml:"transport_descriptors"`
		Participant struct {
			RTPS struct {
				Builtin string `xml:"useBuiltinTransports"`
				User    struct {
					ID string `xml:"transport_id"`
				} `xml:"userTransports"`
			} `xml:"rtps"`
		} `xml:"participant"`
	}
	if err := xml.Unmarshal([]byte(ros2HostFastDDSProfile), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Descriptor.Transport.Type != "UDPv4" || profile.Participant.RTPS.Builtin != "false" || profile.Participant.RTPS.User.ID != "wendy_udp" {
		t.Fatalf("host inspector could use host shared memory: %+v", profile)
	}
}

func TestHostROS2InspectorRejectsMutatingAndExtendedCommands(t *testing.T) {
	for _, args := range [][]string{
		{"topic", "pub", "/cmd_vel"}, {"service", "call", "/move"}, {"action", "send_goal", "/navigate"}, {"param", "set", "/robot", "armed", "true"},
		{"topic", "echo", "/odom", "--qos-reliability", "reliable"}, {"topic", "info", "--help"}, {"topic", "list", "-t", "--verbose"}, nil,
	} {
		if _, err := hostROS2InspectionArgs(args); err == nil {
			t.Errorf("accepted host command: %v", args)
		}
	}
	for _, args := range [][]string{{"topic", "list", "-t"}, {"topic", "info", "-v", "/odom"}, {"topic", "info", "/odom"}, {"topic", "echo", "/odom"}, {"topic", "hz", "/odom"}} {
		got, err := hostROS2InspectionArgs(args)
		if err != nil {
			t.Fatalf("inspection rejected: %v: %v", args, err)
		}
		if args[1] == "list" || args[1] == "info" {
			if !strings.Contains(strings.Join(got, " "), "--no-daemon --spin-time 2") {
				t.Fatalf("inspection may start persistent host DDS daemon: %v", got)
			}
		} else if !reflect.DeepEqual(got, args) {
			t.Fatalf("changed direct subscription: %v", got)
		}
	}
}

func TestROS2AppExecKeepsLocalDiscovery(t *testing.T) {
	if got := ros2ExecDiscoveryEnv(false); !reflect.DeepEqual(got, []string{"ROS_LOCALHOST_ONLY=1", "ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST"}) {
		t.Fatalf("app inspection scope broadened: %v", got)
	}
}
