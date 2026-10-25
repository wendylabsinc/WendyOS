package data

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

func liveSubscriber(t *testing.T, m *Manager, ids ...string) *live.Subscriber {
	t.Helper()
	s, err := m.Live().Subscribe(live.Selection{StreamIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func nextLive(t *testing.T, s *live.Subscriber) live.Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("no live record: %v", err)
	}
	return d
}

func requireNoLive(t *testing.T, s *live.Subscriber) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if d, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected live record %+v %v", d.Record, err)
	}
}

func TestRecordStreamPublishesLive(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := liveSubscriber(t, m)
	markers := appconfig.RecordingStream{Mode: "lightweight", MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"}
	if _, err := m.RecordStream("test.app", "", "markers", markers, &recordingpb.Record{Payload: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	d := nextLive(t, sub)
	if d.Record.StreamID != "apps/test.app/markers" || d.Record.Schema != "wendy.data.v1.VideoMarkers" || !bytes.Equal(d.Record.Payload, []byte{1, 2, 3}) || d.Record.TimestampUnixNanos == 0 {
		t.Fatalf("lightweight record: %+v", d.Record)
	}
	if _, err := m.RecordStream("test.app", "worker", "frames", durableTestStream, testRecord("one")); err != nil {
		t.Fatal(err)
	}
	if d = nextLive(t, sub); d.Record.StreamID != "apps/test.app/worker/frames" || d.Record.Schema != "" || !bytes.Equal(d.Record.Payload, testRecord("").Payload) {
		t.Fatalf("durable record: %+v", d.Record)
	}

	// Nothing the recorder refuses, or already has, reaches a viewer.
	if _, err := m.RecordStream("test.app", "worker", "frames", durableTestStream, testRecord("one")); err != nil {
		t.Fatal(err) // duplicate: acknowledged, not recorded again
	}
	if _, err := m.RecordStream("test.app", "worker", "frames", durableTestStream, testRecord("")); err == nil {
		t.Fatal("durable record without an id accepted")
	}
	if _, err := m.RecordStream("test.app", "", "markers", markers, &recordingpb.Record{Payload: make([]byte, MaxRecordingPacket+1)}); err == nil {
		t.Fatal("oversized record accepted")
	}
	changed := testRecord("one")
	changed.Payload = []byte("reused id")
	if _, err := m.RecordStream("test.app", "worker", "frames", durableTestStream, changed); err == nil {
		t.Fatal("id reused with new content accepted")
	}
	requireNoLive(t, sub)
}

// The app socket reads every lightweight packet into one reused buffer. The
// live record must not alias it, or a later packet would rewrite a record a
// slow viewer has not been sent yet.
func TestRecordStreamLiveDoesNotAliasCallerBuffer(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := liveSubscriber(t, m)
	buf := []byte("first")
	cfg := appconfig.RecordingStream{Mode: "lightweight", MediaType: "text/plain"}
	if _, err := m.RecordStream("test.app", "", "text", cfg, &recordingpb.Record{Payload: buf}); err != nil {
		t.Fatal(err)
	}
	copy(buf, "XXXXX")
	if d := nextLive(t, sub); string(d.Record.Payload) != "first" {
		t.Fatalf("live payload aliases the caller's buffer: %q", d.Record.Payload)
	}
}

// Live and recorded are the same record: the payload a viewer is sent is the
// one the pre-roll ring holds, not a copy, and an open episode records the
// same bytes.
func TestLiveAndRecordedAreTheSameRecord(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	sub := liveSubscriber(t, m, "apps/test.app/worker/frames")
	episode, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordStream("test.app", "worker", "frames", durableTestStream, testRecord("one")); err != nil {
		t.Fatal(err)
	}
	d := nextLive(t, sub)
	m.mu.Lock()
	buffered := m.streamPreRoll[len(m.streamPreRoll)-1].record.Record.Payload
	m.mu.Unlock()
	if &buffered[0] != &d.Record.Payload[0] {
		t.Fatal("live payload is a copy of the recorded record")
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, episode.ID, RecordingLogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recorded, _, err := ReadRecording(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recorded.Record.Payload, d.Record.Payload) {
		t.Fatal("episode and live payloads differ")
	}
	if _, _, err = ReadRecording(f); err != io.EOF {
		t.Fatalf("episode holds more than the one record: %v", err)
	}
}

func TestLiveTimestamp(t *testing.T) {
	const receipt = int64(1_700_000_000_000_000_000)
	nowBoot, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	// A CLOCK_BOOTTIME reading one second ago converts to about one second
	// ago in UNIX time.
	oneSecondAgo := time.Now().Add(-time.Second).UnixNano()
	bootOneSecondAgo := nowBoot - int64(time.Second)
	stored := func(r *recordingpb.Record, accepted bool) *recordingpb.StoredRecord {
		return &recordingpb.StoredRecord{Record: r, ReceiptBootId: "boot-a", ReceiptUnixNanos: receipt, ClientTimestampAccepted: accepted}
	}
	timing := func(clock, boot string, first int64) *recordingpb.SampleTiming {
		return &recordingpb.SampleTiming{Clock: clock, BootId: boot, Count: 2, StartNanos: proto.Int64(first), PeriodNanos: proto.Int64(10)}
	}
	for _, tc := range []struct {
		name   string
		stored *recordingpb.StoredRecord
		want   int64
		near   bool // converted between clocks: compare within a tolerance
	}{
		{"receipt without timing", stored(&recordingpb.Record{}, false), receipt, false},
		{"UNIX timing", stored(&recordingpb.Record{Timing: timing("UNIX", "", 42)}, false), 42, false},
		{"CLOCK_REALTIME explicit timestamps", stored(&recordingpb.Record{Timing: &recordingpb.SampleTiming{Clock: "CLOCK_REALTIME", Count: 2, TimestampsNanos: []int64{7, 9}}}, false), 7, false},
		{"CLOCK_BOOTTIME this boot", stored(&recordingpb.Record{Timing: timing("CLOCK_BOOTTIME", "boot-a", bootOneSecondAgo)}, false), oneSecondAgo, true},
		{"CLOCK_BOOTTIME other boot", stored(&recordingpb.Record{Timing: timing("CLOCK_BOOTTIME", "boot-b", bootOneSecondAgo)}, false), receipt, false},
		{"named device clock", stored(&recordingpb.Record{Timing: timing("imu0", "", 42)}, false), receipt, false},
		{"accepted client boottime", stored(&recordingpb.Record{ClientBoottimeNanos: proto.Int64(bootOneSecondAgo)}, true), oneSecondAgo, true},
		{"refused client boottime", stored(&recordingpb.Record{ClientBoottimeNanos: proto.Int64(bootOneSecondAgo)}, false), receipt, false},
		{"UNIX timing wins over client boottime", stored(&recordingpb.Record{Timing: timing("UNIX", "", 42), ClientBoottimeNanos: proto.Int64(bootOneSecondAgo)}, true), 42, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := liveTimestamp(tc.stored)
			if !tc.near && got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
			if diff := time.Duration(got - tc.want).Abs(); tc.near && diff > 50*time.Millisecond {
				t.Fatalf("converted time off by %v", diff)
			}
		})
	}
}

// The agent stamps lightweight records, which carry no timing, with receipt
// time, so live data and video taken at the same moment line up.
func TestRecordStreamLiveTimestampIsReceiptForLightweight(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := liveSubscriber(t, m)
	before := time.Now().UnixNano()
	if _, err := m.RecordStream("test.app", "", "text", appconfig.RecordingStream{Mode: "lightweight", MediaType: "text/plain"}, &recordingpb.Record{Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixNano()
	if ts := nextLive(t, sub).Record.TimestampUnixNanos; ts < before || ts > after {
		t.Fatalf("timestamp %d outside receipt window %d..%d", ts, before, after)
	}
}

// An app's declared stream is in the catalog from the moment its socket opens,
// and its newest record waits for a viewer that asks for it, even when nobody
// was subscribed while the app wrote it.
func TestAdvertisedAppStreamCatalogAndLatest(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	markers := appconfig.RecordingStream{Mode: "lightweight", MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"}
	m.AdvertiseAppStream("test.app", "worker", "markers", markers)
	for _, payload := range []string{"old", "new"} {
		if _, err := m.RecordStream("test.app", "worker", "markers", markers, &recordingpb.Record{Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}

	s, err := m.Live().Subscribe(live.Selection{StreamPrefixes: []string{live.CatalogPrefix, "apps/test.app/"}, SendLatest: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d := nextLive(t, s)
	var info agentpbv2.DataStreamInfo
	if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
		t.Fatal(err)
	}
	if d.Record.StreamID != "_streams/apps/test.app/worker/markers" || info.GetStreamId() != "apps/test.app/worker/markers" || info.GetPublisher() != "test.app" ||
		info.GetSchema() != "wendy.data.v1.VideoMarkers" || info.GetMediaType() != "application/protobuf" || info.GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE {
		t.Fatalf("catalog entry %s: %v", d.Record.StreamID, &info)
	}
	if d = nextLive(t, s); d.Record.StreamID != "apps/test.app/worker/markers" || string(d.Record.Payload) != "new" {
		t.Fatalf("newest record: %s %q", d.Record.StreamID, d.Record.Payload)
	}
	requireNoLive(t, s)

	m.UnadvertiseAppStream("test.app", "worker", "markers")
	d = nextLive(t, s)
	if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
		t.Fatal(err)
	}
	if info.GetState() != agentpbv2.DataStreamInfo_STATE_GONE {
		t.Fatalf("after the app stopped: %v", &info)
	}
}

// An agent producer's record takes the app stream path: the viewer gets it on
// "agent/<stream>", stamped with the time of the sample it describes, and the
// episode records the same record on that sample's place in its timeline.
func TestPublishAgentRecordLiveAndRecorded(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	const stream = "campaign/door/markers/v4l2:/dev/video0"
	sub := liveSubscriber(t, m, "agent/"+stream)
	episode, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	sampleBoot, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	const sampleUnix = int64(1_792_000_000_123_456_789)
	descriptor := &recordingpb.StreamDescriptor{MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"}
	record := &recordingpb.Record{
		Payload:             []byte("boxes"),
		Timing:              &recordingpb.SampleTiming{Clock: "CLOCK_REALTIME", Count: 1, TimestampsNanos: []int64{sampleUnix}},
		ClientBoottimeNanos: proto.Int64(sampleBoot),
		BootId:              bootID(),
		Inputs:              []*recordingpb.SampleRef{{SourceId: "v4l2:/dev/video0", SampleId: 42}},
	}
	if err := m.Publish(stream, descriptor, record); err != nil {
		t.Fatal(err)
	}
	record.Payload[0] = 'X' // the caller keeps its buffer
	d := nextLive(t, sub)
	if d.Record.StreamID != "agent/"+stream || d.Record.Schema != "wendy.data.v1.VideoMarkers" || string(d.Record.Payload) != "boxes" {
		t.Fatalf("live record: %+v", d.Record)
	}
	if d.Record.TimestampUnixNanos != sampleUnix {
		t.Fatalf("live timestamp %d, want the sample's %d", d.Record.TimestampUnixNanos, sampleUnix)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, episode.ID, RecordingLogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recorded, _, err := ReadRecording(f)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.AppId != "" || recorded.Stream != stream || !bytes.Equal(recorded.Record.Payload, d.Record.Payload) {
		t.Fatalf("recorded: %+v", recorded)
	}
	if bootID() == "unavailable" {
		// Without a kernel boot id (macOS) no client boot time is trusted, and
		// the episode places the record at its receipt.
		if recorded.ClientTimestampAccepted {
			t.Fatal("client boot time accepted without a boot id")
		}
	} else if !recorded.ClientTimestampAccepted || recorded.EpisodeNanos != sampleBoot-episode.RequestBootNanos {
		t.Fatalf("recorded at %d (accepted %v), want the sample's offset %d", recorded.EpisodeNanos, recorded.ClientTimestampAccepted, sampleBoot-episode.RequestBootNanos)
	}
	if in := recorded.Record.GetInputs(); len(in) != 1 || in[0].SampleId != 42 {
		t.Fatalf("recorded inputs: %v", in)
	}
}

// Live goes first: a viewer gets a record while a seal holds the manager
// lock, before the record reaches the pre-roll or an episode.
func TestPublishReachesViewersBeforeStorage(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := liveSubscriber(t, m)
	m.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- m.Publish("test/stream", &recordingpb.StreamDescriptor{MediaType: "text/plain"}, &recordingpb.Record{Payload: []byte("x")})
	}()
	d := nextLive(t, sub)
	buffered := len(m.streamPreRoll)
	m.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d.Record.StreamID != "agent/test/stream" || buffered != 0 {
		t.Fatalf("live record %+v with %d pre-roll records already buffered", d.Record, buffered)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streamPreRoll) != 1 {
		t.Fatalf("pre-roll holds %d records after the publish", len(m.streamPreRoll))
	}
}

// The agent namespace is the agent's: a recording stream with no app id is
// refused, and Publish refuses what an episode could not store.
func TestPublishAndRecordStreamRefusals(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := liveSubscriber(t, m)
	cfg := appconfig.RecordingStream{Mode: "lightweight", MediaType: "text/plain"}
	if _, err := m.RecordStream("", "", "markers", cfg, &recordingpb.Record{Payload: []byte("x")}); err == nil {
		t.Fatal("recording stream without an app id accepted")
	}
	descriptor := &recordingpb.StreamDescriptor{MediaType: "text/plain"}
	for _, tc := range []struct {
		name       string
		stream     string
		descriptor *recordingpb.StreamDescriptor
		record     *recordingpb.Record
	}{
		{"empty stream", "", descriptor, &recordingpb.Record{}},
		{"long stream", strings.Repeat("s", maxAgentStream+1), descriptor, &recordingpb.Record{}},
		{"no descriptor", "s", nil, &recordingpb.Record{}},
		{"no record", "s", descriptor, nil},
		{"oversized", "s", descriptor, &recordingpb.Record{Payload: make([]byte, MaxRecordingPacket)}},
		{"timing without a clock", "s", descriptor, &recordingpb.Record{Timing: &recordingpb.SampleTiming{Count: 1}}},
	} {
		if err := m.Publish(tc.stream, tc.descriptor, tc.record); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	requireNoLive(t, sub)
}

// An agent producer's stream is in the catalog while the producer runs,
// published by "agent", and its newest record waits for a viewer that asks
// for it, as an app's stream does.
func TestAdvertisedAgentStreamCatalogAndLatest(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const stream = "campaign/door/markers/v4l2:/dev/video0"
	descriptor := &recordingpb.StreamDescriptor{MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"}
	m.AdvertiseAgentStream(stream, descriptor)
	for _, payload := range []string{"old", "new"} {
		if err := m.Publish(stream, descriptor, &recordingpb.Record{Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}

	s, err := m.Live().Subscribe(live.Selection{StreamPrefixes: []string{live.CatalogPrefix, "agent/"}, SendLatest: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var info agentpbv2.DataStreamInfo
	d := nextLive(t, s)
	if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
		t.Fatal(err)
	}
	if d.Record.StreamID != live.CatalogPrefix+"agent/"+stream || info.GetStreamId() != "agent/"+stream || info.GetPublisher() != "agent" ||
		info.GetSchema() != "wendy.data.v1.VideoMarkers" || info.GetMediaType() != "application/protobuf" || info.GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE {
		t.Fatalf("catalog entry %s: %v", d.Record.StreamID, &info)
	}
	if d = nextLive(t, s); d.Record.StreamID != "agent/"+stream || string(d.Record.Payload) != "new" {
		t.Fatalf("newest record: %s %q", d.Record.StreamID, d.Record.Payload)
	}
	requireNoLive(t, s)

	m.UnadvertiseAgentStream(stream)
	d = nextLive(t, s)
	if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
		t.Fatal(err)
	}
	if info.GetStreamId() != "agent/"+stream || info.GetState() != agentpbv2.DataStreamInfo_STATE_GONE {
		t.Fatalf("after the producer stopped: %v", &info)
	}
}
