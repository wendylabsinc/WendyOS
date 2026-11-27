//go:build linux

package ros2sensor_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2sensor"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// These tests run the whole path with the real pieces: a simulated SensorLink
// board that offers only a temperature channel, the agent's supervisor and
// bridge, and a ROS 2 node subscribed with rclpy. They need a ROS 2 install,
// so they run through .github/scripts/test-rtps-publisher-ros2.sh and are
// skipped elsewhere.

type tcpDialer struct{}

func (tcpDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// noCameras is the camera loopback of a device whose paired source has no
// camera: nothing is ever mounted.
type noCameras struct{}

func (noCameras) EnsureNode(context.Context, uint32, string) error { return nil }
func (noCameras) NodePath(uint32) (string, bool)                   { return "", false }
func (noCameras) RemoveCamera(uint32)                              {}

func celsius(v float32) []byte {
	return binary.LittleEndian.AppendUint32(nil, math.Float32bits(v))
}

// TestROS2NodeReceivesAPairedTemperatureSensor has the ROS 2 app on the
// host's network, as an app with the host network entitlement is.
func TestROS2NodeReceivesAPairedTemperatureSensor(t *testing.T) {
	subscriber := os.Getenv("WENDY_ROS2_INTEROP_SUBSCRIBER")
	if subscriber == "" {
		t.Skip("needs a ROS 2 install; run .github/scripts/test-rtps-publisher-ros2.sh")
	}
	for i, rmw := range []string{"rmw_cyclonedds_cpp", "rmw_fastrtps_cpp"} {
		t.Run(rmw, func(t *testing.T) { pairedSensorReachesROS2Node(t, subscriber, rmw, 90+i, false) })
	}
}

// TestIsolatedROS2AppReceivesAPairedTemperatureSensor has the ROS 2 app in a
// network namespace of its own, as an app is by default, so the agent has to
// enter that namespace to publish. That takes CAP_SYS_ADMIN; the script runs
// it in a container that has it.
func TestIsolatedROS2AppReceivesAPairedTemperatureSensor(t *testing.T) {
	subscriber := os.Getenv("WENDY_ROS2_INTEROP_SUBSCRIBER")
	if subscriber == "" || os.Getenv("WENDY_ROS2_INTEROP_NETNS") == "" {
		t.Skip("needs a ROS 2 install and the right to enter network namespaces; run .github/scripts/test-rtps-publisher-ros2.sh")
	}
	for i, rmw := range []string{"rmw_cyclonedds_cpp", "rmw_fastrtps_cpp"} {
		t.Run(rmw, func(t *testing.T) { pairedSensorReachesROS2Node(t, subscriber, rmw, 95+i, true) })
	}
}

func pairedSensorReachesROS2Node(t *testing.T, subscriber, rmw string, domain int, isolated bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The board: one generic sensor channel, no camera, no microphone.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go sim.Serve(ctx, ln, sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 3, Name: "chip-temperature",
			Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{
				Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4, RateHz: 50}},
		}}},
		Frames:        [][]byte{celsius(20.5), celsius(21), celsius(21.5)},
		FrameInterval: 20 * time.Millisecond,
		// Like a board, it stamps its readings with a clock of its own: it has
		// been up five seconds and reads its sensor every 20 ms, exactly.
		TsUs: func(seq uint32) uint64 { return 5_000_000 + uint64(seq)*20_000 },
	})

	// The agent. Its only running ROS 2 app is the node started below: on the
	// host's network like this process, or in a network of its own, which the
	// agent learns of once the app is up.
	var appPID atomic.Uint32
	if !isolated {
		appPID.Store(uint32(os.Getpid()))
	}
	pool := rtps.NewPool()
	defer pool.Close()
	apps := func(context.Context) ([]ros2camera.Graph, error) {
		pid := appPID.Load()
		if pid == 0 {
			return nil, nil
		}
		return []ros2camera.Graph{{Key: "app", InstanceKey: "app", DomainID: domain,
			NetworkNamespacePID: pid, HostNetwork: !isolated, SensorRead: true}}, nil
	}
	bridge := ros2sensor.New(zap.NewNop(), pool, apps)
	go bridge.Run(ctx)
	supervisor := mcusource.NewSupervisor(zap.NewNop(), noCameras{},
		func(_ mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
			return mcusource.NewTCPTransport(tcpDialer{}, addr), nil
		},
		func(string) ros2camera.CameraWriter { return nil }, nil)
	supervisor.SetGenericSink(bridge)
	pairing := mcusource.SensorPairing{SourceAssetID: 7, OrgID: 1, Name: "porch-board"}
	go func() { _ = supervisor.RunPairing(ctx, pairing, ln.Addr().String()) }()

	// The app: an ordinary ROS 2 node with a default (reliable) subscription.
	const count = 12
	const topic = "/sensorlink/porch_board/chip_temperature"
	args := []string{subscriber,
		"--topic", topic, "--type", "temperature", "--timing-topic", topic + "/timing",
		"--reliability", "reliable", "--count", strconv.Itoa(count), "--timeout", "60"}
	cmd := exec.CommandContext(ctx, "python3", args...)
	if isolated {
		// A new network namespace starts with its loopback down.
		cmd = exec.CommandContext(ctx, "unshare", append([]string{"--net", "python3"}, append(args, "--bring-up-loopback")...)...)
	}
	// Loopback only unless the script asks for the wider discovery an app
	// with ROS 2 scope "network" has; the agent publishes on loopback either
	// way.
	localhostOnly := os.Getenv("WENDY_ROS2_INTEROP_LOCALHOST_ONLY")
	if localhostOnly == "" {
		localhostOnly = "1"
	}
	cmd.Env = append(os.Environ(), "RMW_IMPLEMENTATION="+rmw, "ROS_DOMAIN_ID="+strconv.Itoa(domain), "ROS_LOCALHOST_ONLY="+localhostOnly)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	type record struct {
		Ready       bool     `json:"ready"`
		Temperature *float64 `json:"temperature"`
		FrameID     string   `json:"frame_id"`
		Sec         int64    `json:"sec"`
		Nanosec     int64    `json:"nanosec"`
		Timing      *struct {
			FrameID    string            `json:"frame_id"`
			Sec        int64             `json:"sec"`
			Nanosec    int64             `json:"nanosec"`
			Statuses   int               `json:"statuses"`
			Level      int               `json:"level"`
			Name       string            `json:"name"`
			Message    string            `json:"message"`
			HardwareID string            `json:"hardware_id"`
			Values     map[string]string `json:"values"`
		} `json:"timing"`
		Graph *struct {
			Types            []string `json:"types"`
			Publishers       int      `json:"publishers"`
			TimingTypes      []string `json:"timing_types"`
			TimingPublishers int      `json:"timing_publishers"`
		} `json:"graph"`
	}
	var records []record
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var r record
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			continue
		}
		if r.Ready && isolated {
			// The app is running: from now on the agent's list of ROS 2 apps
			// names its process, as containerd's does for a real app.
			appPID.Store(uint32(cmd.Process.Pid))
		}
		if r.Temperature != nil || r.Graph != nil || r.Timing != nil {
			records = append(records, r)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ROS 2 node did not receive %d temperature messages: %v (%d records)", count, err, len(records))
	}

	seen := map[float64]bool{}
	var messages int
	var graph *record
	var stamps []time.Time
	var timings []*record
	for i := range records {
		r := &records[i]
		if r.Graph != nil {
			graph = r
			continue
		}
		if r.Timing != nil {
			timings = append(timings, r)
			continue
		}
		stamps = append(stamps, time.Unix(r.Sec, r.Nanosec))
		messages++
		value := *r.Temperature
		if value != 20.5 && value != 21 && value != 21.5 {
			t.Fatalf("message %d carries %v °C, which the board never sent", messages, value)
		}
		seen[value] = true
		if r.FrameID != "sensorlink/porch_board" {
			t.Fatalf("message %d has frame %q", messages, r.FrameID)
		}
	}
	if messages < count || len(seen) != 3 {
		t.Fatalf("received %d messages covering %v, want at least %d covering all three readings", messages, seen, count)
	}

	// The readings carry the time the board took them, moved to this device's
	// clock: close to now although the board's clock counts from its boot, in
	// order, and 20 ms apart as the board took them, not as they arrived. A
	// stamp is a wall-clock time, so the spacing is the board's to well under
	// a microsecond rather than to the nanosecond. The offset between the
	// clocks still settles now and then, which shows as one gap further off.
	spaced := 0
	for i := 1; i < len(stamps); i++ {
		gap := stamps[i].Sub(stamps[i-1])
		if gap <= 0 || gap > 200*time.Millisecond {
			t.Fatalf("reading %d is stamped %s after the one before it", i, gap)
		}
		if off := gap - 20*time.Millisecond; off > -time.Microsecond && off < time.Microsecond {
			spaced++
		}
	}
	if spaced < (len(stamps)-1)*2/3 {
		t.Fatalf("%d of %d readings are stamped 20ms after the one before; stamps: %v", spaced, len(stamps)-1, stamps)
	}
	if age := time.Since(stamps[len(stamps)-1]); age < -time.Second || age > 30*time.Second {
		t.Fatalf("the last reading is stamped %s ago", age)
	}

	// The timing topic says how each of those times is known.
	if len(timings) < count {
		t.Fatalf("received %d timing messages, want at least %d", len(timings), count)
	}
	readingAt := map[time.Time]bool{}
	for _, stamp := range stamps {
		readingAt[stamp] = true
	}
	matched := 0
	for i, r := range timings {
		timing := r.Timing
		if timing.Statuses != 1 || timing.FrameID != "sensorlink/porch_board" || timing.Name != topic || timing.HardwareID != "asset:7" {
			t.Fatalf("timing %d names frame %q, topic %q, source %q in %d statuses", i, timing.FrameID, timing.Name, timing.HardwareID, timing.Statuses)
		}
		for key, want := range map[string]string{
			"readings": "1", "sample_period_nanos": "20000000",
			"native_clock_domain": "SENSORLINK_SOURCE", "native_uncertainty_nanos": "unreported",
			"mapping_segment":             "sensorlink-source-1",
			"timestamp_uncertainty_nanos": "unbounded", "confidence": "unbounded",
		} {
			if timing.Values[key] != want {
				t.Fatalf("timing %d: %s = %q, want %q (all: %v)", i, key, timing.Values[key], want, timing.Values)
			}
		}
		// The board's own time for the frame, as it sent it.
		sequence, _ := strconv.ParseUint(timing.Values["sequence"], 10, 32)
		if want := strconv.FormatUint((5_000_000+sequence*20_000)*1000, 10); timing.Values["native_timestamp_nanos"] != want {
			t.Fatalf("timing %d: frame %d has native_timestamp_nanos %q, want %s", i, sequence, timing.Values["native_timestamp_nanos"], want)
		}
		if _, err := strconv.ParseUint(timing.Values["mapping_uncertainty_nanos"], 10, 63); err != nil {
			t.Fatalf("timing %d: mapping_uncertainty_nanos = %q", i, timing.Values["mapping_uncertainty_nanos"])
		}
		// The board reports no uncertainty, which is a warning.
		if timing.Level != 1 || timing.Message == "" {
			t.Fatalf("timing %d: level %d, message %q", i, timing.Level, timing.Message)
		}
		if readingAt[time.Unix(timing.Sec, timing.Nanosec)] {
			matched++
		}
	}
	// A timing message is stamped like the reading it describes. The two
	// subscriptions do not start on the same frame, so a few have no partner.
	if matched < count/2 {
		t.Fatalf("%d of %d timing messages carry the stamp of a received reading", matched, len(timings))
	}
	if graph == nil || graph.Graph.Publishers < 1 || len(graph.Graph.Types) != 1 || graph.Graph.Types[0] != "sensor_msgs/msg/Temperature" {
		t.Fatalf("the ROS 2 graph reports %+v for the topic, want a publisher of sensor_msgs/msg/Temperature", graph)
	}
	if graph.Graph.TimingPublishers < 1 || len(graph.Graph.TimingTypes) != 1 || graph.Graph.TimingTypes[0] != "diagnostic_msgs/msg/DiagnosticArray" {
		t.Fatalf("the ROS 2 graph reports %+v for the timing topic, want a publisher of diagnostic_msgs/msg/DiagnosticArray", graph.Graph)
	}
	if !supervisor.IsPairingConnected(pairing) {
		t.Fatal("a source that is delivering its sensor is not reported as connected")
	}
}
