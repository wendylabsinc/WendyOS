package robotprobe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// The real lease must keep satisfying the seam, or the reader silently stops being
// usable against an actual DDS domain while its tests still pass.
var _ Lease = (*rtps.Lease)(nil)

type fakeLease struct {
	endpoints  []rtps.Endpoint
	samples    chan rtps.Sample
	done       chan struct{}
	subscribed []rtps.GUID
	subErr     error
}

func newFakeLease(endpoints ...rtps.Endpoint) *fakeLease {
	return &fakeLease{
		endpoints: endpoints,
		samples:   make(chan rtps.Sample, 16),
		done:      make(chan struct{}),
	}
}

func (f *fakeLease) Endpoints() []rtps.Endpoint  { return f.endpoints }
func (f *fakeLease) Samples() <-chan rtps.Sample { return f.samples }
func (f *fakeLease) Done() <-chan struct{}       { return f.done }
func (f *fakeLease) Subscribe(ep rtps.Endpoint) error {
	if f.subErr != nil {
		return f.subErr
	}
	f.subscribed = append(f.subscribed, ep.GUID)
	return nil
}

func guid(n byte) rtps.GUID {
	return rtps.GUID{Prefix: rtps.GUIDPrefix{n}, EntityID: uint32(n)}
}

func writer(n byte, topic, typeName string) rtps.Endpoint {
	return rtps.Endpoint{GUID: guid(n), Topic: topic, Type: typeName}
}

// ROS 2 publishes a topic as "rt" plus its name. A caller should be able to ask for the
// name an operator would type and still find it.
func TestDDSReaderResolvesEitherSpellingOfATopic(t *testing.T) {
	lease := newFakeLease(writer(1, "rt/camera/color/camera_info", rosmsg.TypeCameraInfo))
	reader := NewDDSReader(lease)

	for _, asked := range []string{"/camera/color/camera_info", "rt/camera/color/camera_info"} {
		lease.samples <- rtps.Sample{Writer: guid(1), Payload: []byte("payload")}
		payloads, err := reader.Sample(context.Background(), asked, rosmsg.TypeCameraInfo, time.Second, 1)
		if err != nil {
			t.Fatalf("%s: %v", asked, err)
		}
		if len(payloads) != 1 {
			t.Errorf("%s returned %d payloads, want 1", asked, len(payloads))
		}
	}
}

// A lease carries one sample channel for every subscription on it. Crediting another
// topic's messages to this one would report a rate no publisher produced, which is the
// opposite of what a measurement is for.
func TestDDSReaderCreditsOnlyTheRequestedWriter(t *testing.T) {
	wanted := writer(1, "rt/camera/color/image_raw", "sensor_msgs::msg::dds_::Image_")
	other := writer(2, "rt/lowstate", "unitree_go::msg::dds_::LowState_")
	lease := newFakeLease(wanted, other)
	reader := NewDDSReader(lease)

	for i := 0; i < 3; i++ {
		lease.samples <- rtps.Sample{Writer: other.GUID, Payload: []byte("not ours")}
	}
	lease.samples <- rtps.Sample{Writer: wanted.GUID, Payload: []byte("ours")}

	payloads, err := reader.Sample(context.Background(), "/camera/color/image_raw", wanted.Type, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 || string(payloads[0]) != "ours" {
		t.Fatalf("got %d payloads %q, want exactly the requested writer's", len(payloads), payloads)
	}
}

func TestDDSReaderStopsAtMaxMessages(t *testing.T) {
	ep := writer(1, "rt/camera/color/image_raw", "sensor_msgs::msg::dds_::Image_")
	lease := newFakeLease(ep)
	for i := 0; i < 10; i++ {
		lease.samples <- rtps.Sample{Writer: ep.GUID, Payload: []byte{byte(i)}}
	}

	payloads, err := NewDDSReader(lease).Sample(context.Background(), "/camera/color/image_raw", ep.Type, time.Second, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 4 {
		t.Errorf("got %d payloads, want 4", len(payloads))
	}
}

func TestDDSReaderCollectsEverythingInTheWindowWhenUnbounded(t *testing.T) {
	ep := writer(1, "rt/camera/color/image_raw", "sensor_msgs::msg::dds_::Image_")
	lease := newFakeLease(ep)
	for i := 0; i < 5; i++ {
		lease.samples <- rtps.Sample{Writer: ep.GUID, Payload: []byte{byte(i)}}
	}

	// The window bounds the wait; a rate measurement needs every message in it.
	payloads, err := NewDDSReader(lease).Sample(context.Background(), "/camera/color/image_raw", ep.Type, 50*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 5 {
		t.Errorf("got %d payloads, want all 5 published in the window", len(payloads))
	}
}

// A topic nobody publishes on is a finding for the probe to report, not an error to
// propagate — the distinction the whole UNKNOWN vocabulary rests on.
func TestDDSReaderIsSilentAboutAnAbsentTopic(t *testing.T) {
	reader := NewDDSReader(newFakeLease(writer(1, "rt/lowstate", "unitree_go::msg::dds_::LowState_")))
	payloads, err := reader.Sample(context.Background(), "/camera/color/camera_info", rosmsg.TypeCameraInfo, time.Millisecond, 1)
	if err != nil {
		t.Errorf("absent topic returned an error: %v", err)
	}
	if payloads != nil {
		t.Errorf("got %v, want no payloads", payloads)
	}
}

// A writer advertising a different type is not the topic we asked for, even when the
// name matches.
func TestDDSReaderRequiresTheTypeToMatchWhenGiven(t *testing.T) {
	reader := NewDDSReader(newFakeLease(writer(1, "rt/camera/color/camera_info", "some_other::msg::dds_::Thing_")))
	payloads, err := reader.Sample(context.Background(), "/camera/color/camera_info", rosmsg.TypeCameraInfo, time.Millisecond, 1)
	if err != nil || payloads != nil {
		t.Errorf("payloads=%v err=%v, want neither; the type did not match", payloads, err)
	}
}

func TestDDSReaderSubscribesOncePerWriter(t *testing.T) {
	ep := writer(1, "rt/camera/color/camera_info", rosmsg.TypeCameraInfo)
	lease := newFakeLease(ep)
	reader := NewDDSReader(lease)

	for i := 0; i < 3; i++ {
		if _, err := reader.Sample(context.Background(), "/camera/color/camera_info", ep.Type, time.Millisecond, 1); err != nil {
			t.Fatal(err)
		}
	}
	if len(lease.subscribed) != 1 {
		t.Errorf("subscribed %d times, want 1; repeated sampling must not stack subscriptions", len(lease.subscribed))
	}
}

func TestDDSReaderReportsASubscribeFailure(t *testing.T) {
	lease := newFakeLease(writer(1, "rt/camera/color/camera_info", rosmsg.TypeCameraInfo))
	lease.subErr = errors.New("reader already disposed")
	reader := NewDDSReader(lease)

	if _, err := reader.Sample(context.Background(), "/camera/color/camera_info", rosmsg.TypeCameraInfo, time.Second, 1); err == nil {
		t.Error("a subscribe failure was swallowed")
	}
}

func TestDDSReaderReportsAClosedParticipant(t *testing.T) {
	lease := newFakeLease(writer(1, "rt/camera/color/camera_info", rosmsg.TypeCameraInfo))
	close(lease.done)
	reader := NewDDSReader(lease)

	if _, err := reader.Sample(context.Background(), "/camera/color/camera_info", rosmsg.TypeCameraInfo, time.Second, 1); err == nil {
		t.Error("sampling a closed participant reported success")
	}
}

func TestDDSReaderKeepsWhatArrivedBeforeCancellation(t *testing.T) {
	ep := writer(1, "rt/camera/color/image_raw", "sensor_msgs::msg::dds_::Image_")
	lease := newFakeLease(ep)
	lease.samples <- rtps.Sample{Writer: ep.GUID, Payload: []byte("first")}

	ctx, cancel := context.WithCancel(context.Background())
	reader := NewDDSReader(lease)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	payloads, err := reader.Sample(ctx, "/camera/color/image_raw", ep.Type, time.Minute, 0)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(payloads) != 1 {
		t.Errorf("got %d payloads, want the one that arrived before cancellation", len(payloads))
	}
}

// Discovery, so a caller can ask which cameras a robot has instead of being told.
func TestTopicsOfTypeListsRosNamesWithoutDuplicates(t *testing.T) {
	reader := NewDDSReader(newFakeLease(
		writer(1, "rt/camera/color/camera_info", rosmsg.TypeCameraInfo),
		writer(2, "rt/camera/depth/camera_info", rosmsg.TypeCameraInfo),
		writer(3, "rt/camera/color/camera_info", rosmsg.TypeCameraInfo), // a second publisher
		writer(4, "rt/lowstate", "unitree_go::msg::dds_::LowState_"),
	))

	topics := reader.TopicsOfType(rosmsg.TypeCameraInfo)
	if len(topics) != 2 {
		t.Fatalf("got %v, want the two distinct camera_info topics", topics)
	}
	for _, want := range []string{"/camera/color/camera_info", "/camera/depth/camera_info"} {
		if !contains(topics, want) {
			t.Errorf("missing %q in %v; names must read as an operator would type them", want, topics)
		}
	}
}
