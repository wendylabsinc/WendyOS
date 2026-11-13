package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFoxgloveApp(t *testing.T) {
	dir := t.TempDir()
	opts := foxgloveServeOpts{domain: 3, rmw: "rmw_cyclonedds_cpp", distro: "humble"}
	if err := writeFoxgloveApp(dir, opts); err != nil {
		t.Fatalf("writeFoxgloveApp: %v", err)
	}

	df, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dfs := string(df)
	for _, want := range []string{
		"FROM ros:humble",
		"ros-humble-foxglove-bridge",
		"export ROS_LOCALHOST_ONLY=0 ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET",
		"ros2 launch foxglove_bridge foxglove_bridge_launch.xml port:=8765 address:=127.0.0.1 include_hidden:=true",
	} {
		if !strings.Contains(dfs, want) {
			t.Fatalf("Dockerfile missing %q:\n%s", want, dfs)
		}
	}
	if strings.Contains(dfs, "address:=0.0.0.0") {
		t.Fatalf("Foxglove WebSocket unexpectedly exposed on every device interface:\n%s", dfs)
	}

	wj, err := os.ReadFile(filepath.Join(dir, "wendy.json"))
	if err != nil {
		t.Fatal(err)
	}
	wjs := string(wj)
	for _, want := range []string{
		`"domainId": 3`,
		`"rmw": "rmw_cyclonedds_cpp"`,
		`"distro": "humble"`,
		`"scope": "network"`,
		`"type": "network", "mode": "host"`,
	} {
		if !strings.Contains(wjs, want) {
			t.Fatalf("wendy.json missing %q:\n%s", want, wjs)
		}
	}
	if strings.Contains(wjs, `"services"`) {
		t.Fatalf("generated bridge must stay on the single-container chunk-diff deploy path:\n%s", wjs)
	}
}

func TestFoxgloveServeCommandOffersBuilderFlag(t *testing.T) {
	flag := newFoxgloveServeCmd().Flags().Lookup("builder")
	if flag == nil {
		t.Fatal("serve command is missing --builder")
	}
	if flag.DefValue != "" {
		t.Fatalf("builder default = %q, want automatic selection", flag.DefValue)
	}
}

func TestWriteFoxgloveAppDistroTemplated(t *testing.T) {
	dir := t.TempDir()
	if err := writeFoxgloveApp(dir, foxgloveServeOpts{domain: 0, rmw: "rmw_fastrtps_cpp", distro: "jazzy"}); err != nil {
		t.Fatal(err)
	}
	df, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if !strings.Contains(string(df), "FROM ros:jazzy") || !strings.Contains(string(df), "ros-jazzy-foxglove-bridge") {
		t.Fatalf("distro not templated into Dockerfile:\n%s", df)
	}
}

func TestWriteFoxgloveAppCycloneDDSInterface(t *testing.T) {
	dir := t.TempDir()
	opts := foxgloveServeOpts{domain: 0, rmw: "rmw_cyclonedds_cpp", distro: "humble", iface: "enP8p1s0"}
	if err := writeFoxgloveApp(dir, opts); err != nil {
		t.Fatal(err)
	}
	df, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	for _, want := range []string{
		`NetworkInterface name=\"lo\" multicast=\"true\"`,
		`NetworkInterface name=\"enP8p1s0\"`,
		`<ParticipantIndex>auto</ParticipantIndex>`,
		`<Peer address=\"127.0.0.1\"/>`,
	} {
		if !strings.Contains(string(df), want) {
			t.Fatalf("CycloneDDS interface override missing %q:\n%s", want, df)
		}
	}
}

func TestWriteFoxgloveAppRejectsUnsafeInterface(t *testing.T) {
	dir := t.TempDir()
	opts := foxgloveServeOpts{domain: 0, rmw: "rmw_cyclonedds_cpp", distro: "humble", iface: `eth0'; touch /tmp/pwned`}
	if err := writeFoxgloveApp(dir, opts); err == nil {
		t.Fatal("expected unsafe interface name to be rejected")
	}
}
