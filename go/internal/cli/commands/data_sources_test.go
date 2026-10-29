package commands

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func src(id, kind, clock string, healthy bool, detail string) *agentpbv2.DataSource {
	return &agentpbv2.DataSource{Id: id, Kind: kind, ClockDomain: clock, Healthy: healthy, Detail: detail}
}

// named is src from an agent that sends a name and, when unhealthy, a reason.
func named(id, kind string, healthy bool, name, reason, detail string) *agentpbv2.DataSource {
	return &agentpbv2.DataSource{Id: id, Kind: kind, ClockDomain: "CLOCK", Healthy: healthy, Detail: detail, Name: name, UnusableReason: reason}
}

// mesh1Before is `wendy data sources --json` from Mesh 1 (a Jetson Orin Nano,
// agent 2026.10.27-091653), verbatim: an agent that predates names, reasons
// and stable microphone IDs.
const mesh1Before = `{"sources":[{"id":"applications","kind":"application","clock_domain":"CLOCK_BOOTTIME","healthy":true},{"id":"audio:54","kind":"audio","clock_domain":"PIPEWIRE_CAPTURE/AGENT_RECEIPT","healthy":true,"detail":"C920 PRO HD Webcam Analog Stereo alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo"},{"id":"audio:56","kind":"audio","clock_domain":"PIPEWIRE_CAPTURE/AGENT_RECEIPT","healthy":true,"detail":"Built-in Audio Analog Stereo alsa_input.platform-sound.analog-stereo"},{"id":"ros2:rmw_fastrtps_cpp:domain-0","kind":"ros2","clock_domain":"ROSBAG2_STORAGE/ROS_MESSAGE_HEADER/SIM_TIME","healthy":true,"detail":"rmw_fastrtps_cpp DDS domain 0, 2 topics"},{"id":"ros2:rmw_fastrtps_cpp:domain-0:/parameter_events","kind":"ros2","clock_domain":"ROSBAG2_STORAGE/ROS_MESSAGE_HEADER/SIM_TIME","healthy":true,"detail":"rcl_interfaces/msg/ParameterEvent"},{"id":"ros2:rmw_fastrtps_cpp:domain-0:/rosout","kind":"ros2","clock_domain":"ROSBAG2_STORAGE/ROS_MESSAGE_HEADER/SIM_TIME","healthy":true,"detail":"rcl_interfaces/msg/Log"},{"id":"telemetry","kind":"telemetry","clock_domain":"CLOCK_BOOTTIME","healthy":true},{"id":"v4l2:/dev/video0","kind":"camera","clock_domain":"V4L2_BUFFER_TIMESTAMP","healthy":true,"detail":"HD Pro Webcam C920 VIDEO_TRANSPORT_USB"}]}`

func decodeSources(t *testing.T, raw string) []*agentpbv2.DataSource {
	t.Helper()
	var response agentpbv2.DataSourcesResponse
	if err := protojson.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	return response.GetSources()
}

// mesh1After is the same device as this agent lists it.
func mesh1After() []*agentpbv2.DataSource {
	const ros2Clock = "ROSBAG2_STORAGE/ROS_MESSAGE_HEADER/SIM_TIME"
	sources := []*agentpbv2.DataSource{
		named("applications", "application", true, "Application records", "", ""),
		named("audio:alsa_input.platform-sound.analog-stereo", "audio", true, "Built-in Audio", "", "Built-in Audio Analog Stereo alsa_input.platform-sound.analog-stereo"),
		named("audio:alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo", "audio", true, "C920 PRO HD Webcam", "", "C920 PRO HD Webcam Analog Stereo alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo"),
		named("ros2:rmw_fastrtps_cpp:domain-0", "ros2", true, "Domain 0 (Fast DDS), all topics", "", "rmw_fastrtps_cpp DDS domain 0, 2 topics"),
		named("ros2:rmw_fastrtps_cpp:domain-0:/parameter_events", "ros2", true, "/parameter_events", "", "rcl_interfaces/msg/ParameterEvent"),
		named("ros2:rmw_fastrtps_cpp:domain-0:/rosout", "ros2", true, "/rosout", "", "rcl_interfaces/msg/Log"),
		named("telemetry", "telemetry", true, "Device telemetry", "", ""),
		named("v4l2:/dev/video0", "camera", true, "HD Pro Webcam C920", "", "HD Pro Webcam C920 VIDEO_TRANSPORT_USB"),
	}
	for _, s := range sources {
		if s.GetKind() == "ros2" {
			s.ClockDomain = ros2Clock
		}
	}
	return sources
}

// orinNanoSources mirrors `wendy data sources` from a Jetson Orin Nano on the
// ALSA path: one USB webcam, one USB microphone, and 20 internal APE ADMAIF
// audio-DMA routing channels that bury everything else.
func orinNanoSources() []*agentpbv2.DataSource {
	sources := []*agentpbv2.DataSource{
		named("applications", "application", true, "Application records", "", ""),
		named("audio:alsa:C920:0", "audio", true, "HD Pro Webcam C920", "", "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio] plughw:0,0"),
	}
	for i := 0; i < 20; i++ {
		sources = append(sources, named(
			fmt.Sprintf("audio:alsa:APE:%d", i), "audio", false, fmt.Sprintf("NVIDIA Jetson Orin Nano APE, device %d", i), "silent audio-hub channel",
			fmt.Sprintf("APE [NVIDIA Jetson Orin Nano APE], device %d: fe.admaif@290f000.ADMAIF%d (*) [] plughw:2,%d", i, i+1, i)))
	}
	return append(sources,
		named("telemetry", "telemetry", true, "Device telemetry", "", ""),
		named("v4l2:/dev/video0", "camera", true, "HD Pro Webcam C920", "", "HD Pro Webcam C920 VIDEO_TRANSPORT_USB"))
}

func renderSources(t *testing.T, sources []*agentpbv2.DataSource, all bool, kinds ...string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeDataSources(&buf, sources, sourceListing{kinds: normalizeSourceKinds(kinds), all: all}); err != nil {
		t.Fatalf("writeDataSources: %v", err)
	}
	return buf.String()
}

// TestDataSourcesMesh1 is the whole table for the device that prompted this
// layout, as a person reads it.
func TestDataSourcesMesh1(t *testing.T) {
	got := renderSources(t, mesh1After(), false)
	want := `  CAMERAS                          STATUS     ID
* HD Pro Webcam C920               connected  v4l2:/dev/video0

  MICROPHONES                      STATUS     ID
* Built-in Audio                   detected   audio:alsa_input.platform-sound.analog-stereo
* C920 PRO HD Webcam               detected   audio:alsa_input.usb-046d_HD_Pro_Webcam_C920_C2DD0F9F-02.analog-stereo

  ROS 2                            STATUS     ID
* Domain 0 (Fast DDS), all topics  reachable  ros2:rmw_fastrtps_cpp:domain-0

  OTHER                            STATUS     ID
* Application records              built in   applications
* Device telemetry                 built in   telemetry

* recorded by ` + "`wendy data record`" + ` when no --source is given
2 ROS 2 housekeeping topics hidden (/parameter_events, /rosout); they are still recorded with their domain. --all lists them.

Record some: wendy data record --source <ID> [--source <ID> ...]
More:        wendy data sources --json
`
	if got != want {
		t.Fatalf("table changed:\n got:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasSuffix(line, " ") || strings.Contains(line, "\t") {
			t.Errorf("line has trailing space or a tab: %q", line)
		}
	}
	if strings.Contains(got, "CLOCK") || strings.Contains(got, "ROSBAG2_STORAGE") {
		t.Errorf("the clock domain belongs in --json, not the table:\n%s", got)
	}
}

// TestDataSourcesOlderAgentFallsBackToDetail renders the real "before" answer
// from Mesh 1: no names, no reasons. NAME falls back to detail and STATUS to
// the agent's own words, and the table says why.
func TestDataSourcesOlderAgentFallsBackToDetail(t *testing.T) {
	got := renderSources(t, decodeSources(t, mesh1Before), false)
	want := `  CAMERAS                                   STATUS   ID
* HD Pro Webcam C920 VIDEO_TRANSPORT_USB    healthy  v4l2:/dev/video0

  MICROPHONES                               STATUS   ID
* C920 PRO HD Webcam Analog Stereo alsa...  healthy  audio:54
* Built-in Audio Analog Stereo alsa_inp...  healthy  audio:56

  ROS 2                                     STATUS   ID
* rmw_fastrtps_cpp DDS domain 0, 2 topics   healthy  ros2:rmw_fastrtps_cpp:domain-0

  OTHER                                     STATUS   ID
* applications                              healthy  applications
* telemetry                                 healthy  telemetry

* recorded by ` + "`wendy data record`" + ` when no --source is given
2 ROS 2 housekeeping topics hidden (/parameter_events, /rosout); they are still recorded with their domain. --all lists them.
This device's agent predates source names: NAME shows its raw detail, and STATUS its own healthy or unhealthy.

Record some: wendy data record --source <ID> [--source <ID> ...]
More:        wendy data sources --json
`
	if got != want {
		t.Fatalf("older agent's table changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDataSourcesGroupOrder(t *testing.T) {
	sources := []*agentpbv2.DataSource{
		named("applications", "application", true, "Application records", "", ""),
		named("lidar:0", "lidar", true, "Front lidar", "", ""),
		named("ros2:rmw_cyclonedds_cpp:domain-42", "ros2", true, "Domain 42 (Cyclone DDS), all topics", "", ""),
		named("audio:alsa:C920:0", "audio", true, "HD Pro Webcam C920", "", ""),
		named("v4l2:/dev/video0", "camera", true, "HD Pro Webcam C920", "", ""),
	}
	got := renderSources(t, sources, false)
	last := -1
	for _, header := range []string{"  CAMERAS", "  MICROPHONES", "  ROS 2", "  OTHER"} {
		at := strings.Index(got, header)
		if at < 0 || at < last {
			t.Fatalf("group %q missing or out of order:\n%s", header, got)
		}
		last = at
	}
	// A kind this CLI does not know lands in OTHER rather than vanishing, and
	// keeps the generic status word.
	other := got[strings.Index(got, "  OTHER"):]
	if !strings.Contains(other, "Front lidar") || !strings.Contains(other, "healthy") {
		t.Errorf("unknown kind not listed under OTHER:\n%s", got)
	}

	// Empty groups are not printed at all.
	if cameras := renderSources(t, sources, false, "camera"); strings.Contains(cameras, "MICROPHONES") || strings.Contains(cameras, "OTHER") {
		t.Errorf("--kind camera printed other groups:\n%s", cameras)
	}
}

func TestDataSourceStatusWords(t *testing.T) {
	for _, tc := range []struct {
		source *agentpbv2.DataSource
		want   string
	}{
		{named("v4l2:/dev/video0", "camera", true, "C920", "", ""), "connected"},
		{named("ipcamera:200", "camera", true, "RLC-520A", "", ""), "online"},
		{named("ipcamera:201", "camera", false, "RLC-520A", "no login stored", ""), "no login stored"},
		{named("ipcamera:202", "camera", false, "RLC-810A", "camera offline", ""), "camera offline"},
		{named("audio:alsa:C920:0", "audio", true, "C920", "", ""), "detected"},
		{named("audio:alsa:APE:0", "audio", false, "APE", "silent audio-hub channel", ""), "silent audio-hub channel"},
		{named("ros2:rmw_fastrtps_cpp:domain-0", "ros2", true, "Domain 0", "", ""), "reachable"},
		{named("ros2:rmw_fastrtps_cpp:domain-0", "ros2", false, "Domain 0", "topic listing failed", ""), "topic listing failed"},
		{named("ros2:rmw_fastrtps_cpp:domain-0:/scan", "ros2", true, "/scan", "", ""), "listed"},
		{named("applications", "application", true, "Application records", "", ""), "built in"},
		{named("telemetry", "telemetry", true, "Device telemetry", "", ""), "built in"},
		{named("lidar:0", "lidar", true, "Lidar", "", ""), "healthy"},
		{named("lidar:1", "lidar", false, "Lidar", "", ""), "unhealthy"},
		// An older agent: its own words, whatever the kind.
		{src("v4l2:/dev/video0", "camera", "V4L2_BUFFER_TIMESTAMP", true, "C920"), "healthy"},
		{src("ipcamera:200", "camera", "IP", false, "RLC (credentials required)"), "unhealthy"},
	} {
		if got := sourceStatus(tc.source); got != tc.want {
			t.Errorf("sourceStatus(%s healthy=%v) = %q, want %q", tc.source.GetId(), tc.source.GetHealthy(), got, tc.want)
		}
	}
}

// TestDataSourcesMarksDefaultRecording checks the marker follows the agent's
// default-selection rule: unusable sources are left out unless named.
func TestDataSourcesMarksDefaultRecording(t *testing.T) {
	got := renderSources(t, []*agentpbv2.DataSource{
		named("ipcamera:200", "camera", false, "RLC-520A", "no login stored", ""),
		named("v4l2:/dev/video0", "camera", true, "HD Pro Webcam C920", "", ""),
	}, false)
	for _, want := range []string{
		"  RLC-520A            no login stored  ipcamera:200",
		"* HD Pro Webcam C920  connected        v4l2:/dev/video0",
		"* recorded by `wendy data record` when no --source is given",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	// With nothing recordable there is no marker, so no legend either.
	none := renderSources(t, []*agentpbv2.DataSource{named("ipcamera:200", "camera", false, "RLC-520A", "no login stored", "")}, false)
	if strings.Contains(none, "* recorded") {
		t.Errorf("legend printed with no marked source:\n%s", none)
	}
}

func TestDataSourcesROS2Housekeeping(t *testing.T) {
	sources := append(mesh1After(), named("ros2:rmw_fastrtps_cpp:domain-0:/scan", "ros2", true, "/scan", "", "sensor_msgs/msg/LaserScan"))

	hidden := renderSources(t, sources, false)
	if !strings.Contains(hidden, "ros2:rmw_fastrtps_cpp:domain-0:/scan") {
		t.Errorf("a real topic was hidden:\n%s", hidden)
	}
	for _, topic := range ros2HousekeepingTopics {
		if strings.Contains(hidden, "domain-0:"+topic) {
			t.Errorf("housekeeping topic %s listed without --all:\n%s", topic, hidden)
		}
	}

	all := renderSources(t, sources, true)
	for _, topic := range ros2HousekeepingTopics {
		if !strings.Contains(all, "domain-0:"+topic) {
			t.Errorf("--all did not list %s:\n%s", topic, all)
		}
	}
	if strings.Contains(all, "housekeeping") {
		t.Errorf("--all still reports hidden topics:\n%s", all)
	}
	// A listed topic says what was checked about it.
	if !strings.Contains(all, "* /rosout") || !strings.Contains(all, "listed     ros2:rmw_fastrtps_cpp:domain-0:/rosout") {
		t.Errorf("topic row not rendered as expected:\n%s", all)
	}
}

// TestDataSourcesLongIDsDoNotWidenTheTable pins the reason the ID is the last
// column: the longest ID on the device pads nothing, and a long name is capped.
func TestDataSourcesLongIDsDoNotWidenTheTable(t *testing.T) {
	topic := "/robot/sensors/front_stereo/left/image_rect_color/compressed"
	long := "ros2:rmw_fastrtps_cpp:domain-0:" + topic
	got := renderSources(t, append(mesh1After(), named(long, "ros2", true, topic, "", "")), false)
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "v4l2:/dev/video0") {
			if !strings.HasSuffix(line, "v4l2:/dev/video0") {
				t.Errorf("something follows the ID column: %q", line)
			}
			if at := strings.Index(line, "connected"); at > 2+maxSourceNameWidth+2 {
				t.Errorf("STATUS starts at column %d, past the name cap: %q", at, line)
			}
		}
	}
	if !strings.Contains(got, "/robot/sensors/front_stereo/left/imag...") {
		t.Errorf("a long name was not capped at %d runes:\n%s", maxSourceNameWidth, got)
	}
	if !strings.Contains(got, long+"\n") {
		t.Errorf("the long ID was not printed whole:\n%s", got)
	}
}

func TestDataSourcesFloodBoundary(t *testing.T) {
	manyMics := func(n int) []*agentpbv2.DataSource {
		var out []*agentpbv2.DataSource
		for i := 0; i < n; i++ {
			out = append(out, named(fmt.Sprintf("audio:alsa:X:%d", i), "audio", true, fmt.Sprintf("Mic %d", i), "", ""))
		}
		return out
	}
	cases := []struct {
		name        string
		count       int
		kinds       []string
		all         bool
		wantRows    int
		wantContain []string
		wantAbsent  []string
	}{
		{name: "exactly the limit lists every row", count: sourceKindFloodLimit, wantRows: sourceKindFloodLimit, wantAbsent: []string{"more microphone"}},
		{name: "one over the limit", count: sourceKindFloodLimit + 1, wantRows: sourceKindFloodLimit,
			wantContain: []string{"... 1 more microphone not listed (--all or --kind audio lists them)"}},
		{name: "jetson flood", count: 21, wantRows: sourceKindFloodLimit,
			wantContain: []string{"... 15 more microphones not listed (--all or --kind audio lists them)"}},
		{name: "--kind lifts the limit", count: 21, kinds: []string{"AUDIO"}, wantRows: 21, wantAbsent: []string{"more microphones"}},
		{name: "--all lifts the limit", count: 21, all: true, wantRows: 21, wantAbsent: []string{"more microphones"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderSources(t, manyMics(c.count), c.all, c.kinds...)
			if rows := strings.Count(got, "audio:alsa:X:"); rows != c.wantRows {
				t.Fatalf("listed %d rows, want %d:\n%s", rows, c.wantRows, got)
			}
			for _, want := range c.wantContain {
				if !strings.Contains(got, want) {
					t.Fatalf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(got, absent) {
					t.Fatalf("output unexpectedly contains %q:\n%s", absent, got)
				}
			}
		})
	}

	// The flood never hides another kind, and an unusable audio-hub channel
	// shows its reason where it is listed.
	got := renderSources(t, orinNanoSources(), false)
	for _, want := range []string{"v4l2:/dev/video0", "audio:alsa:C920:0", "applications", "telemetry", "silent audio-hub channel", "... 15 more microphones not listed"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestDataSourcesEmptyAndUnknownKind(t *testing.T) {
	if got := renderSources(t, nil, false); got != "No recordable sources reported by the device.\n" {
		t.Errorf("empty listing = %q", got)
	}
	got := renderSources(t, orinNanoSources(), false, "lidar")
	if !strings.Contains(got, `No sources of kind "lidar".`) || !strings.Contains(got, "Kinds present on this device: application, audio, telemetry, camera.") {
		t.Errorf("unknown kind message = %q", got)
	}
}

// --json without --kind must stay the full response the device sent, in the
// canonical protobuf JSON dialect every data subcommand emits, because scripts
// consume it. The new fields only ever add to it.
func TestDataSourcesJSON(t *testing.T) {
	response := &agentpbv2.DataSourcesResponse{Sources: append(mesh1After(), orinNanoSources()...)}

	var want bytes.Buffer
	if err := encodeProtoJSON(&want, response); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got bytes.Buffer
	if err := encodeDataSourcesJSON(&got, response, normalizeSourceKinds(nil)); err != nil {
		t.Fatalf("encodeDataSourcesJSON: %v", err)
	}
	if got.String() != want.String() {
		t.Fatalf("json output changed\n got: %s\nwant: %s", got.String(), want.String())
	}
	for _, field := range []string{`"clock_domain":"ROSBAG2_STORAGE/ROS_MESSAGE_HEADER/SIM_TIME"`, `"name":"C920 PRO HD Webcam"`, `"unusable_reason":"silent audio-hub channel"`, `domain-0:/rosout`, `"healthy":true`, `"detail":"`} {
		if !strings.Contains(got.String(), field) {
			t.Errorf("json output missing %s", field)
		}
	}

	var filtered bytes.Buffer
	if err := encodeDataSourcesJSON(&filtered, response, normalizeSourceKinds([]string{"camera"})); err != nil {
		t.Fatalf("encodeDataSourcesJSON: %v", err)
	}
	if strings.Contains(filtered.String(), "audio:") || !strings.Contains(filtered.String(), "v4l2:/dev/video0") {
		t.Fatalf("--kind camera --json = %s", filtered.String())
	}

	// An older agent's answer passes through untouched: no field appears that
	// it did not send.
	var old bytes.Buffer
	if err := encodeDataSourcesJSON(&old, &agentpbv2.DataSourcesResponse{Sources: decodeSources(t, mesh1Before)}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(old.String()) != mesh1Before {
		t.Fatalf("older agent's json changed:\n got: %s\nwant: %s", old.String(), mesh1Before)
	}
}

func TestNormalizeSourceKinds(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "lowercases and trims", in: []string{" Camera ", "AUDIO"}, want: []string{"camera", "audio"}},
		{name: "drops empties from comma splits", in: []string{"camera", "", "  "}, want: []string{"camera"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSourceKinds(c.in)
			if strings.Join(got, ",") != strings.Join(c.want, ",") || len(got) != len(c.want) {
				t.Fatalf("normalizeSourceKinds(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// sourcesClient answers Sources from a fixed list and counts the calls.
type sourcesClient struct {
	agentpbv2.DataServiceClient
	sources []*agentpbv2.DataSource
	calls   int
}

func (c *sourcesClient) Sources(context.Context, *agentpbv2.DataSourcesRequest, ...grpc.CallOption) (*agentpbv2.DataSourcesResponse, error) {
	c.calls++
	return &agentpbv2.DataSourcesResponse{Sources: c.sources}, nil
}

func TestWarnLegacyMicrophoneIDs(t *testing.T) {
	// A current agent lists stable IDs, so audio:54 is a deprecated alias.
	current := &sourcesClient{sources: mesh1After()}
	var out bytes.Buffer
	warnLegacyMicrophoneIDs(context.Background(), &out, current, []string{"audio:54", "telemetry", "audio:54"})
	if strings.Count(out.String(), "Warning: audio:54 is an old-style microphone ID") != 1 || !strings.Contains(out.String(), "wendy data sources --kind audio") {
		t.Errorf("warning = %q, want exactly one, pointing at the stable IDs", out.String())
	}

	// An older agent still lists audio:54 as the ID itself: nothing to warn.
	older := &sourcesClient{sources: decodeSources(t, mesh1Before)}
	out.Reset()
	warnLegacyMicrophoneIDs(context.Background(), &out, older, []string{"audio:54"})
	if out.Len() != 0 {
		t.Errorf("warned against an agent that predates stable IDs: %q", out.String())
	}

	// No old-style ID typed: the device is not even asked.
	quiet := &sourcesClient{sources: mesh1After()}
	warnLegacyMicrophoneIDs(context.Background(), &out, quiet, []string{"audio:alsa_input.platform-sound.analog-stereo", "v4l2:/dev/video0"})
	if quiet.calls != 0 || out.Len() != 0 {
		t.Errorf("made %d Sources calls and printed %q for stable IDs", quiet.calls, out.String())
	}
}
