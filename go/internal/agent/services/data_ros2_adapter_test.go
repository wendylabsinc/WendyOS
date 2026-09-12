package services

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

const testROS2TopicList = "/camera/left/image_raw [sensor_msgs/msg/Image]\n" +
	"/chatter [std_msgs/msg/String]\n" +
	"/parameter_events [rcl_interfaces/msg/ParameterEvent]\n" +
	"/rosout [rcl_interfaces/msg/Log]\n"

// recordingROS2Runtime answers `topic list -t` and pretends to record a bag,
// remembering the argument vectors so a test can assert what rosbag2 was told
// to record.
type recordingROS2Runtime struct {
	*fakeROS2Runtime
	mu         sync.Mutex
	recordArgs [][]string
	listCalls  int
	listErr    bool
}

func newRecordingROS2Runtime(sidecars ...ROS2Sidecar) *recordingROS2Runtime {
	r := &recordingROS2Runtime{fakeROS2Runtime: &fakeROS2Runtime{sidecars: sidecars}}
	r.fakeROS2Runtime.execFn = r.exec
	return r
}

func (r *recordingROS2Runtime) exec(ctx context.Context, opts ROS2ExecOptions, stdout, stderr io.Writer) (int, error) {
	joined := strings.Join(opts.Args, " ")
	switch {
	case joined == "topic list -t":
		r.mu.Lock()
		r.listCalls++
		failing := r.listErr
		r.mu.Unlock()
		if failing {
			_, _ = io.WriteString(stderr, "failed to create node: rcl_init failed")
			return 1, nil
		}
		_, _ = io.WriteString(stdout, testROS2TopicList)
		return 0, nil
	case joined == "topic list":
		// The clock sampler's /clock probe; no simulated time in these tests.
		return 0, nil
	case len(opts.Args) >= 5 && opts.Args[0] == "bag" && opts.Args[1] == "record":
		r.mu.Lock()
		r.recordArgs = append(r.recordArgs, append([]string(nil), opts.Args...))
		r.mu.Unlock()
		output := opts.Args[3]
		if err := os.MkdirAll(output, 0o750); err != nil {
			return 1, err
		}
		if err := os.WriteFile(filepath.Join(output, "metadata.yaml"), []byte("rosbag2_bagfile_information:\n"), 0o640); err != nil {
			return 1, err
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	fmt.Fprintf(stderr, "unknown command: ros2 %s", joined)
	return 1, nil
}

func (r *recordingROS2Runtime) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.recordArgs...)
}

func (r *recordingROS2Runtime) enumerations() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls
}

func (r *recordingROS2Runtime) failListing(failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listErr = failing
}

func testSidecar() ROS2Sidecar {
	return ROS2Sidecar{Name: "sc-cyc", RMW: "rmw_cyclonedds_cpp", DomainID: 42}
}

func testAdapter(t *testing.T, runtime ROS2Runtime) *ros2DataAdapter {
	t.Helper()
	return newROS2Adapter(NewROS2Service(nil, runtime, t.TempDir()))
}

func sourceByID(sources []data.Source, id string) (data.Source, bool) {
	for _, source := range sources {
		if source.ID == id {
			return source, true
		}
	}
	return data.Source{}, false
}

// TestROS2DiscoverEmitsOneSourcePerTopic is the headline behavior: a robot's
// ROS 2 graph is addressable topic by topic, and the DETAIL column carries the
// message type so a person can tell which topic they meant.
func TestROS2DiscoverEmitsOneSourcePerTopic(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	sources := testAdapter(t, runtime).Discover(context.Background())

	wantDetail := map[string]string{
		"ros2:rmw_cyclonedds_cpp:domain-42:/camera/left/image_raw": "sensor_msgs/msg/Image",
		"ros2:rmw_cyclonedds_cpp:domain-42:/chatter":               "std_msgs/msg/String",
		"ros2:rmw_cyclonedds_cpp:domain-42:/parameter_events":      "rcl_interfaces/msg/ParameterEvent",
		"ros2:rmw_cyclonedds_cpp:domain-42:/rosout":                "rcl_interfaces/msg/Log",
	}
	for id, detail := range wantDetail {
		source, ok := sourceByID(sources, id)
		if !ok {
			t.Fatalf("topic source %s missing from %+v", id, sources)
		}
		if source.Detail != detail {
			t.Errorf("%s detail = %q, want the message type %q", id, source.Detail, detail)
		}
		if source.Kind != "ros2" {
			t.Errorf("%s kind = %q, want ros2", id, source.Kind)
		}
		if !source.Healthy {
			t.Errorf("%s is not healthy but enumeration succeeded", id)
		}
		if source.ClockDomain != ros2ClockDomain {
			t.Errorf("%s clock domain = %q, want %q", id, source.ClockDomain, ros2ClockDomain)
		}
	}
	// The whole-domain handle stays listed: it is what a campaign written
	// before per-topic sources existed names, and what "record everything"
	// selects.
	domain, ok := sourceByID(sources, "ros2:rmw_cyclonedds_cpp:domain-42")
	if !ok {
		t.Fatalf("domain-level source missing from %+v", sources)
	}
	if !strings.Contains(domain.Detail, "4 topics") {
		t.Errorf("domain detail = %q, want it to report the topic count", domain.Detail)
	}
	if len(sources) != len(wantDetail)+1 {
		t.Errorf("Discover returned %d sources, want %d", len(sources), len(wantDetail)+1)
	}
	// One `ros2 topic list -t` answers names and types together; nothing else
	// may be run during discovery, and `ros2 topic info` in particular is a
	// per-topic exec that would cost ten seconds on a hundred-topic robot.
	for _, call := range runtime.fakeROS2Runtime.calls {
		if joined := strings.Join(call.Args, " "); joined != "topic list -t" {
			t.Errorf("Discover ran an unexpected command: ros2 %s", joined)
		}
	}
}

// TestROS2DiscoverDerivesHealthFromEnumeration pins the fix for a Healthy that
// was hardcoded true, so a dead DDS domain enumerated as healthy.
func TestROS2DiscoverDerivesHealthFromEnumeration(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	runtime.failListing(true)
	sources := testAdapter(t, runtime).Discover(context.Background())

	if len(sources) != 1 {
		t.Fatalf("failed enumeration yielded %d sources, want only the domain: %+v", len(sources), sources)
	}
	source := sources[0]
	if source.ID != "ros2:rmw_cyclonedds_cpp:domain-42" {
		t.Fatalf("source id = %q", source.ID)
	}
	if source.Healthy {
		t.Fatal("a domain whose topic listing failed reported itself healthy")
	}
	if !strings.Contains(source.Detail, "listing topics failed") || !strings.Contains(source.Detail, "rcl_init failed") {
		t.Errorf("detail = %q, want it to name the enumeration failure", source.Detail)
	}
}

// TestROS2DiscoverCachesWithinTTL proves the cache actually prevents a second
// enumeration, that the TTL expires it, and that invalidation is explicit.
func TestROS2DiscoverCachesWithinTTL(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	adapter := testAdapter(t, runtime)
	now := time.Now()
	adapter.now = func() time.Time { return now }

	first := adapter.Discover(context.Background())
	if got := runtime.enumerations(); got != 1 {
		t.Fatalf("first Discover ran %d enumerations, want 1", got)
	}
	second := adapter.Discover(context.Background())
	if got := runtime.enumerations(); got != 1 {
		t.Fatalf("second Discover inside the TTL ran %d enumerations, want the cached 1", got)
	}
	if len(first) != len(second) {
		t.Fatalf("cached listing differs: %d then %d sources", len(first), len(second))
	}

	now = now.Add(ros2DiscoveryTTL + time.Millisecond)
	adapter.Discover(context.Background())
	if got := runtime.enumerations(); got != 2 {
		t.Fatalf("Discover past the TTL ran %d enumerations, want 2", got)
	}

	adapter.invalidateDiscovery()
	adapter.Discover(context.Background())
	if got := runtime.enumerations(); got != 3 {
		t.Fatalf("Discover after explicit invalidation ran %d enumerations, want 3", got)
	}

	// A sidecar that changes identity must miss regardless of the TTL: the
	// cached listing describes a graph that is no longer the one being asked
	// about.
	runtime.fakeROS2Runtime.sidecars = []ROS2Sidecar{{Name: "sc-cyc", RMW: "rmw_cyclonedds_cpp", DomainID: 7}}
	adapter.Discover(context.Background())
	if got := runtime.enumerations(); got != 4 {
		t.Fatalf("Discover after a domain change ran %d enumerations, want 4", got)
	}
}

func startROS2Capture(t *testing.T, adapter *ros2DataAdapter, selected []data.Source) ([]data.CaptureResult, string) {
	t.Helper()
	episodeRoot := t.TempDir()
	capture, err := adapter.Start(context.Background(), data.CaptureSession{ID: "episode-test", Directory: episodeRoot}, selected)
	if err != nil {
		t.Fatal(err)
	}
	if capture == nil {
		t.Fatal("Start returned no capture for a selection that named ROS 2 sources")
	}
	results, err := capture.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return results, episodeRoot
}

// TestROS2CaptureRecordsOnlyTheNamedTopics is the point of the change: naming
// two topics must not record the whole domain.
func TestROS2CaptureRecordsOnlyTheNamedTopics(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	adapter := testAdapter(t, runtime)
	sources := adapter.Discover(context.Background())
	chatter, _ := sourceByID(sources, "ros2:rmw_cyclonedds_cpp:domain-42:/chatter")
	image, _ := sourceByID(sources, "ros2:rmw_cyclonedds_cpp:domain-42:/camera/left/image_raw")

	results, episodeRoot := startROS2Capture(t, adapter, []data.Source{chatter, image})

	recorded := runtime.recorded()
	// One recorder for the domain, not one per topic: each rosbag2 process
	// pays its own DDS discovery and produces its own bag and clock mapping.
	if len(recorded) != 1 {
		t.Fatalf("started %d recorders for two topics on one domain, want 1: %v", len(recorded), recorded)
	}
	args := recorded[0]
	if got := strings.Join(args[4:], " "); got != "/camera/left/image_raw /chatter" {
		t.Fatalf("recorder topics = %q, want the two named topics", got)
	}
	for _, arg := range args {
		if arg == "-a" {
			t.Fatalf("a per-topic selection recorded the whole domain: %v", args)
		}
	}
	// Both named sources are accounted for in the manifest even though one
	// recorder produced one bag.
	if len(results) != 2 {
		t.Fatalf("results = %+v, want one per selected source", results)
	}
	ids := map[string]bool{results[0].SourceID: true, results[1].SourceID: true}
	if !ids[chatter.ID] || !ids[image.ID] {
		t.Fatalf("results named %v, want %s and %s", ids, chatter.ID, image.ID)
	}
	// One bag per domain, named by the domain rather than by any one topic.
	metadata := filepath.Join(episodeRoot, "ros2", safeCaptureName("ros2:rmw_cyclonedds_cpp:domain-42"), "metadata.yaml")
	if _, err := os.Stat(metadata); err != nil {
		t.Fatalf("moved bag missing: %v", err)
	}
}

// TestROS2CaptureDomainIDStillRecordsEverything is the backwards-compatibility
// case: campaign YAML deployed before per-topic sources existed names the
// domain, and it must still get `ros2 bag record -a`.
func TestROS2CaptureDomainIDStillRecordsEverything(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	adapter := testAdapter(t, runtime)
	sources := adapter.Discover(context.Background())
	domain, ok := sourceByID(sources, "ros2:rmw_cyclonedds_cpp:domain-42")
	if !ok {
		t.Fatal("domain-level source is no longer discoverable, which breaks deployed campaigns")
	}

	results, episodeRoot := startROS2Capture(t, adapter, []data.Source{domain})

	recorded := runtime.recorded()
	if len(recorded) != 1 {
		t.Fatalf("started %d recorders, want 1: %v", len(recorded), recorded)
	}
	if got := strings.Join(recorded[0][4:], " "); got != "-a" {
		t.Fatalf("domain-level selection recorded %q, want -a", got)
	}
	if len(results) != 1 || results[0].SourceID != domain.ID || results[0].DropAccounting != "unavailable" {
		t.Fatalf("results = %+v", results)
	}
	metadata := filepath.Join(episodeRoot, "ros2", safeCaptureName(domain.ID), "metadata.yaml")
	if _, err := os.Stat(metadata); err != nil {
		t.Fatalf("moved bag missing: %v", err)
	}
}

// TestROS2CaptureDomainSelectionSubsumesTopics covers the default episode,
// which selects every healthy source: the domain and all of its topics. That
// must stay one `-a` recorder rather than one recorder for the domain plus one
// per topic writing the same messages twice.
func TestROS2CaptureDomainSelectionSubsumesTopics(t *testing.T) {
	runtime := newRecordingROS2Runtime(testSidecar())
	adapter := testAdapter(t, runtime)
	sources := adapter.Discover(context.Background())

	results, _ := startROS2Capture(t, adapter, sources)

	recorded := runtime.recorded()
	if len(recorded) != 1 {
		t.Fatalf("started %d recorders, want 1: %v", len(recorded), recorded)
	}
	if got := strings.Join(recorded[0][4:], " "); got != "-a" {
		t.Fatalf("recorded %q, want -a to subsume the per-topic sources", got)
	}
	if len(results) != len(sources) {
		t.Fatalf("results = %d, want one per selected source (%d)", len(results), len(sources))
	}
}

// TestROS2CaptureRejectsUnknownSourceID keeps an identifier this adapter does
// not understand from silently recording nothing.
func TestROS2CaptureRejectsUnknownSourceID(t *testing.T) {
	adapter := testAdapter(t, newRecordingROS2Runtime(testSidecar()))
	_, err := adapter.Start(context.Background(), data.CaptureSession{ID: "e", Directory: t.TempDir()}, []data.Source{{ID: "ros2:not-an-identifier", Kind: "ros2"}})
	if err == nil || !strings.Contains(err.Error(), "unrecognized ROS 2 source id") {
		t.Fatalf("err = %v, want an unrecognized source id error", err)
	}
}

func TestROS2RecordArgsSortAndDeduplicateTopics(t *testing.T) {
	args := ros2RecordArgs("/bags/x", &ros2DomainSelection{topics: []string{"/b", "/a", "/b"}})
	if got := strings.Join(args, " "); got != "bag record -o /bags/x /a /b" {
		t.Fatalf("args = %q", got)
	}
}
