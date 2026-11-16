package mcusource

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const (
	nothingDeliverableMessage = "sensor source has no channel that can be delivered to apps; only cameras (MJPEG or H.264) and microphones (16-bit PCM) are delivered today"
	someUndeliveredMessage    = "sensor source has channels that are not delivered to apps; only cameras (MJPEG or H.264) and microphones (16-bit PCM) are delivered today"
	allowlistMismatchMessage  = "sensor source has no channel named in this pairing's sensor allowlist; nothing is delivered to apps"
	emptyManifestMessage      = "sensor source reports no sensor channels; nothing is delivered to apps"
)

func genericSensor(channel uint32, name, schema string) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: channel, Name: name,
		Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: schema, RateHz: 1, SampleBytes: 4}}}
}

func videoChannel(channel uint32, name string, codec sensorlinkpb.VideoFormat_Codec) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: channel, Name: name,
		Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: codec}}}
}

func audioChannel(channel uint32, name string, codec sensorlinkpb.AudioFormat_Codec) *sensorlinkpb.SensorDescriptor {
	return &sensorlinkpb.SensorDescriptor{ChannelId: channel, InputId: channel, Name: name,
		Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: codec, SampleRate: 16000, Channels: 1}}}
}

// undeliveredSupervisor mounts into the channel-selection fakes and records
// what the supervisor logs at info level and above.
func undeliveredSupervisor(tr SensorTransport) (*Supervisor, *observer.ObservedLogs) {
	core, logs := observer.New(zap.InfoLevel)
	cam := &selectionCamera{}
	s := NewSupervisor(zap.New(core), cam, func(SensorPairing, string) (SensorTransport, error) { return tr, nil },
		func(string) ros2camera.CameraWriter { return cam }, &selectionAudio{})
	return s, logs
}

// loggedNotices returns the log entries that explain undelivered channels.
func loggedNotices(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterMessageSnippet("delivered to apps").All()
}

func assertNotice(t *testing.T, got observer.LoggedEntry, level zapcore.Level, message string, fields map[string]string) {
	t.Helper()
	if got.Level != level || got.Message != message {
		t.Errorf("logged %s %q, want %s %q", got.Level, got.Message, level, message)
	}
	logged := got.ContextMap()
	if len(logged) != len(fields) {
		t.Errorf("logged fields %v, want %v", logged, fields)
	}
	for key, want := range fields {
		if logged[key] != want {
			t.Errorf("logged %s=%v, want %s", key, logged[key], want)
		}
	}
}

func TestSupervisorWarnsOnceWhenSourceOffersNothingDeliverable(t *testing.T) {
	// cleanupTransport panics on Stream: nothing here may be subscribed to.
	tr := &cleanupTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
		audioChannel(5, "voice", sensorlinkpb.AudioFormat_OPUS),
		genericSensor(3, "temperature", "wendy.temperature.celsius.f32le.v1"),
		videoChannel(4, "thermal", sensorlinkpb.VideoFormat_Codec(99)),
		videoChannel(6, "raw", sensorlinkpb.VideoFormat_CODEC_UNSPECIFIED),
		genericSensor(8, "mystery", ""),
		{ChannelId: 7, Name: "blank"},
	}}}
	s, logs := undeliveredSupervisor(tr)
	p := SensorPairing{SourceAssetID: 7, Name: "porch-board"}
	for attempt := 0; attempt < 3; attempt++ { // what the retry loop repeats
		if delivered, err := s.streamOnce(context.Background(), p, "source"); delivered || err != nil {
			t.Fatalf("attempt %d: delivered=%v, error=%v", attempt, delivered, err)
		}
	}
	notices := loggedNotices(logs)
	if len(notices) != 1 {
		t.Fatalf("logged %d notices across 3 attempts, want 1: %v", len(notices), notices)
	}
	assertNotice(t, notices[0], zap.WarnLevel, nothingDeliverableMessage, map[string]string{
		"source": "asset:7",
		"name":   "porch-board",
		"ignored_channels": `"temperature" (channel 3): sensor, schema "wendy.temperature.celsius.f32le.v1"; ` +
			`"thermal" (channel 4): video, unsupported codec 99; ` +
			`"voice" (channel 5): audio, unsupported codec OPUS; ` +
			`"raw" (channel 6): video, no codec declared; ` +
			`"blank" (channel 7): no format declared; ` +
			`"mystery" (channel 8): sensor, no schema declared`,
	})
}

func TestSupervisorReportsUndeliveredChannelsOnceAtInfoWhenOthersMount(t *testing.T) {
	tr := &selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
		videoChannel(1, "cam", sensorlinkpb.VideoFormat_MJPEG),
		genericSensor(2, "imu", "wendy.imu.v1"),
		audioChannel(3, "mic", sensorlinkpb.AudioFormat_PCM_S16LE),
	}}}
	s, logs := undeliveredSupervisor(tr)
	// An unnamed pairing is identified by its key alone.
	p := SensorPairing{SourceAssetID: 9}
	for attempt := 0; attempt < 3; attempt++ {
		if delivered, err := s.streamOnce(context.Background(), p, "source"); !delivered || err != nil {
			t.Fatalf("attempt %d: delivered=%v, error=%v", attempt, delivered, err)
		}
	}
	notices := loggedNotices(logs)
	if len(notices) != 1 {
		t.Fatalf("logged %d notices across 3 attempts, want 1: %v", len(notices), notices)
	}
	assertNotice(t, notices[0], zap.InfoLevel, someUndeliveredMessage, map[string]string{
		"source":           "asset:9",
		"ignored_channels": `"imu" (channel 2): sensor, schema "wendy.imu.v1"`,
	})
}

// A Wendy Lite board offers several modes of one input and the supervisor
// mounts one of them. The other modes are alternatives, not ignored channels.
func TestSupervisorDoesNotReportUnusedModesOfAMountedInput(t *testing.T) {
	large := videoChannel(4, "cam-large", sensorlinkpb.VideoFormat_MJPEG)
	small := videoChannel(2, "cam-small", sensorlinkpb.VideoFormat_MJPEG)
	large.InputId, small.InputId = 0, 0
	tr := &selectionTransport{exclusive: true, manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
		large, small, genericSensor(5, "temperature", "wendy.temperature.celsius.f32le.v1"),
	}}}
	s, logs := undeliveredSupervisor(tr)
	p := SensorPairing{SourcePrincipalURI: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Transport: "wendycom", Name: "lite"}
	if delivered, err := s.streamOnce(context.Background(), p, "source"); !delivered || err != nil {
		t.Fatalf("delivered=%v, error=%v", delivered, err)
	}
	if !slices.Equal(tr.channels, []uint32{2}) {
		t.Fatalf("subscribed channels=%v, want the lowest camera mode only", tr.channels)
	}
	notices := loggedNotices(logs)
	if len(notices) != 1 {
		t.Fatalf("logged %d notices, want 1: %v", len(notices), notices)
	}
	assertNotice(t, notices[0], zap.InfoLevel, someUndeliveredMessage, map[string]string{
		"source":           p.SourcePrincipalURI,
		"name":             "lite",
		"ignored_channels": `"temperature" (channel 5): sensor, schema "wendy.temperature.celsius.f32le.v1"`,
	})
}

func TestSupervisorLogsNothingWhenEveryChannelIsDelivered(t *testing.T) {
	tr := &selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
		videoChannel(1, "cam", sensorlinkpb.VideoFormat_H264),
		audioChannel(2, "mic", sensorlinkpb.AudioFormat_PCM_S16LE),
	}}}
	s, logs := undeliveredSupervisor(tr)
	if delivered, err := s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 9}, "source"); !delivered || err != nil {
		t.Fatalf("delivered=%v, error=%v", delivered, err)
	}
	if all := logs.All(); len(all) != 0 {
		t.Fatalf("logged %v for a fully delivered source", all)
	}
}

func TestSupervisorReportsUndeliveredChannelsAgainOnlyWhenTheyChange(t *testing.T) {
	imu := genericSensor(2, "imu", "wendy.imu.v1")
	temperature := genericSensor(3, "temperature", "wendy.temperature.celsius.f32le.v1")
	cam := videoChannel(1, "cam", sensorlinkpb.VideoFormat_MJPEG)
	tr := &selectionTransport{}
	s, logs := undeliveredSupervisor(tr)
	p := SensorPairing{SourceAssetID: 7}
	const (
		imuOnly   = `"imu" (channel 2): sensor, schema "wendy.imu.v1"`
		imuAndTmp = imuOnly + `; "temperature" (channel 3): sensor, schema "wendy.temperature.celsius.f32le.v1"`
	)
	for i, step := range []struct {
		name    string
		pairing SensorPairing
		sensors []*sensorlinkpb.SensorDescriptor
		// The notice this attempt adds; an empty message means it adds none.
		level   zapcore.Level
		message string
		ignored string
	}{
		{name: "first attempt", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{imu},
			level: zap.WarnLevel, message: nothingDeliverableMessage, ignored: imuOnly},
		{name: "retry", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{imu}},
		{name: "another channel appears", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{imu, temperature},
			level: zap.WarnLevel, message: nothingDeliverableMessage, ignored: imuAndTmp},
		// Manifest order is not a change: sources reorder channels across restarts.
		{name: "manifest reordered", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{temperature, imu}},
		{name: "a camera appears", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{cam, temperature, imu},
			level: zap.InfoLevel, message: someUndeliveredMessage, ignored: imuAndTmp},
		{name: "retry with camera", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{cam, temperature, imu}},
		// The same channels are ignored, but now the source delivers nothing.
		{name: "the camera disappears", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{temperature, imu},
			level: zap.WarnLevel, message: nothingDeliverableMessage, ignored: imuAndTmp},
		{name: "everything is delivered", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{cam}},
		{name: "the sensor returns", pairing: p, sensors: []*sensorlinkpb.SensorDescriptor{imu},
			level: zap.WarnLevel, message: nothingDeliverableMessage, ignored: imuOnly},
		// What one source was told about says nothing about another.
		{name: "another source", pairing: SensorPairing{SourceAssetID: 8}, sensors: []*sensorlinkpb.SensorDescriptor{imu},
			level: zap.WarnLevel, message: nothingDeliverableMessage, ignored: imuOnly},
	} {
		before := len(loggedNotices(logs))
		tr.manifest = &sensorlinkpb.SensorManifest{Sensors: step.sensors}
		if _, err := s.streamOnce(context.Background(), step.pairing, "source"); err != nil {
			t.Fatalf("step %d (%s): %v", i, step.name, err)
		}
		added := loggedNotices(logs)[before:]
		if step.message == "" {
			if len(added) != 0 {
				t.Errorf("step %d (%s): logged again without a change: %v", i, step.name, added)
			}
			continue
		}
		if len(added) != 1 {
			t.Errorf("step %d (%s): logged %d notices, want 1: %v", i, step.name, len(added), added)
			continue
		}
		assertNotice(t, added[0], step.level, step.message, map[string]string{"source": step.pairing.Key(), "ignored_channels": step.ignored})
	}
}

func TestSupervisorDoesNotReportChannelsTheAllowlistExcludes(t *testing.T) {
	sensors := []*sensorlinkpb.SensorDescriptor{
		videoChannel(1, "cam", sensorlinkpb.VideoFormat_MJPEG),
		genericSensor(2, "imu", "wendy.imu.v1"),
		audioChannel(3, "voice", sensorlinkpb.AudioFormat_OPUS),
	}
	const imuOnly = `"imu" (channel 2): sensor, schema "wendy.imu.v1"`
	for _, tc := range []struct {
		name      string
		allow     []string
		delivered bool
		level     zapcore.Level
		message   string
		fields    map[string]string
	}{
		{name: "excluded channels are the user's choice", allow: []string{"cam"}, delivered: true},
		{name: "an allowed sensor beside a camera", allow: []string{"cam", "imu"}, delivered: true,
			level: zap.InfoLevel, message: someUndeliveredMessage, fields: map[string]string{"ignored_channels": imuOnly}},
		{name: "only an undeliverable sensor is allowed", allow: []string{"imu"},
			level: zap.WarnLevel, message: nothingDeliverableMessage, fields: map[string]string{"ignored_channels": imuOnly}},
		{name: "the allowlist names no offered channel", allow: []string{"camera", "mic"},
			level: zap.WarnLevel, message: allowlistMismatchMessage,
			fields: map[string]string{"sensor_allowlist": `"camera", "mic"`, "offered_channels": `"cam", "imu", "voice"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: slices.Clone(sensors)}}
			s, logs := undeliveredSupervisor(tr)
			p := SensorPairing{SourceAssetID: 7, SensorAllowlist: tc.allow}
			for attempt := 0; attempt < 2; attempt++ {
				if delivered, err := s.streamOnce(context.Background(), p, "source"); delivered != tc.delivered || err != nil {
					t.Fatalf("attempt %d: delivered=%v, error=%v", attempt, delivered, err)
				}
				// The retry sees the same channels in another order.
				slices.Reverse(tr.manifest.Sensors)
			}
			notices := loggedNotices(logs)
			if tc.message == "" {
				if len(notices) != 0 {
					t.Fatalf("reported channels the allowlist excludes: %v", notices)
				}
				return
			}
			if len(notices) != 1 {
				t.Fatalf("logged %d notices across 2 attempts, want 1: %v", len(notices), notices)
			}
			tc.fields["source"] = "asset:7"
			assertNotice(t, notices[0], tc.level, tc.message, tc.fields)
		})
	}
}

func TestSupervisorWarnsOnceWhenSourceReportsNoChannels(t *testing.T) {
	tr := &cleanupTransport{manifest: &sensorlinkpb.SensorManifest{}}
	s, logs := undeliveredSupervisor(tr)
	p := SensorPairing{SourceAssetID: 7, Name: "porch-board", SensorAllowlist: []string{"cam"}}
	for attempt := 0; attempt < 2; attempt++ {
		if delivered, err := s.streamOnce(context.Background(), p, "source"); delivered || err != nil {
			t.Fatalf("attempt %d: delivered=%v, error=%v", attempt, delivered, err)
		}
	}
	notices := loggedNotices(logs)
	if len(notices) != 1 {
		t.Fatalf("logged %d notices across 2 attempts, want 1: %v", len(notices), notices)
	}
	assertNotice(t, notices[0], zap.WarnLevel, emptyManifestMessage, map[string]string{"source": "asset:7", "name": "porch-board"})
}

func TestUndeliveredChannelListIsBounded(t *testing.T) {
	var sensors []*sensorlinkpb.SensorDescriptor
	for channel := uint32(1); channel <= 20; channel++ {
		sensors = append(sensors, genericSensor(channel, "s", "wendy.imu.v1"))
	}
	// A source chooses these strings; control characters must not reach the log.
	sensors[0].Name = "first\x1b[2J\n" + strings.Repeat("\x00", 200)
	tr := &cleanupTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: sensors}}
	s, logs := undeliveredSupervisor(tr)
	if _, err := s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 7}, "source"); err != nil {
		t.Fatal(err)
	}
	notices := loggedNotices(logs)
	if len(notices) != 1 {
		t.Fatalf("logged %d notices, want 1", len(notices))
	}
	ignored, _ := notices[0].ContextMap()["ignored_channels"].(string)
	if want := `"first\x1b[2J\n` + strings.Repeat(`\x00`, 54) + `" (channel 1): sensor, schema "wendy.imu.v1"; `; !strings.HasPrefix(ignored, want) {
		t.Errorf("name not escaped and cut to 64 characters: %q", ignored)
	}
	if want := `; "s" (channel 16): sensor, schema "wendy.imu.v1"; and 4 more`; !strings.HasSuffix(ignored, want) {
		t.Errorf("list not cut after 16 channels: %q", ignored)
	}
}

// retryingTransport is a source with one generic sensor that signals every
// manifest fetch, so a test can follow the supervisor's retries.
type retryingTransport struct {
	cleanupTransport
	fetched chan struct{}
}

func (tr *retryingTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	select {
	case tr.fetched <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{genericSensor(1, "temperature", "wendy.temperature.celsius.f32le.v1")}}, nil
}

func TestRetriesReportUndeliveredChannelsOnceUntilUnpaired(t *testing.T) {
	fetched := make(chan struct{})
	core, logs := observer.New(zap.InfoLevel)
	sup := NewSupervisor(zap.New(core), nil, func(SensorPairing, string) (SensorTransport, error) {
		return &retryingTransport{fetched: fetched}, nil
	}, nil, nil)
	runner := NewRunner(zap.NewNop(), sup)
	p := SensorPairing{SourceAssetID: 7, Name: "porch-board"}
	awaitFetch := func(what string) {
		t.Helper()
		select {
		case <-fetched:
		case <-time.After(10 * time.Second):
			t.Fatalf("supervisor did not fetch the manifest (%s)", what)
		}
	}

	runner.Start(p, "source")
	awaitFetch("first attempt")
	awaitFetch("retry after backoff")
	runner.StopPairing(p) // unpair; waits for the retry to finish
	if notices := loggedNotices(logs); len(notices) != 1 {
		t.Fatalf("logged %d notices across a retry, want 1: %v", len(notices), notices)
	}

	// Pairing the same source again is a new start: it is told again.
	runner.Start(p, "source")
	awaitFetch("after pairing again")
	runner.StopPairing(p)
	notices := loggedNotices(logs)
	if len(notices) != 2 {
		t.Fatalf("logged %d notices after unpairing and pairing again, want 2: %v", len(notices), notices)
	}
	for _, notice := range notices {
		assertNotice(t, notice, zap.WarnLevel, nothingDeliverableMessage, map[string]string{
			"source": "asset:7", "name": "porch-board",
			"ignored_channels": `"temperature" (channel 1): sensor, schema "wendy.temperature.celsius.f32le.v1"`,
		})
	}
}
