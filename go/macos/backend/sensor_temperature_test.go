package backend

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"math"
	"testing"
	"time"
)

type temperatureLite struct {
	*fakeDesktopLite
	frame          *sensorlinkpb.SensorData
	subscribeErr   error
	unsubscribeErr error
	unsubscribed   bool
}

func (f *temperatureLite) GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error) {
	return &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 42, Name: "chip-temperature", Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4, RateHz: 1}}}}}, nil
}
func (f *temperatureLite) SensorLinkSubscribe(ids []uint32, _ time.Duration) error {
	if f.frame != nil {
		f.mu.Lock()
		listener := f.listener
		f.mu.Unlock()
		listener(f.frame)
	}
	return f.subscribeErr
}
func (f *temperatureLite) SensorLinkUnsubscribe([]uint32, time.Duration) error {
	f.unsubscribed = true
	return f.unsubscribeErr
}
func temperatureFixture() *temperatureLite {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, math.Float32bits(35.5))
	return &temperatureLite{fakeDesktopLite: newFakeDesktopLite(), frame: &sensorlinkpb.SensorData{ChannelId: 42, FrameSeq: 7, TsUs: 12345, Flags: 3, Payload: payload}}
}
func TestSensorTemperatureReceivesEarlySampleAndRetiresSubscription(t *testing.T) {
	f := temperatureFixture()
	got, err := readSensorTemperature(context.Background(), f)
	if err != nil || got.Name != "chip-temperature" || got.Celsius != 35.5 || got.TimestampUs != 12345 || got.Sequence != 7 {
		t.Fatalf("sample=%+v error=%v", got, err)
	}
	if !f.unsubscribed || f.listener != nil {
		t.Fatal("sample retained its source subscription or listener")
	}
}
func TestSensorTemperatureFailureAndCancellationRetireSubscription(t *testing.T) {
	for _, name := range []string{"malformed", "nonfinite", "subscribe", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			f := temperatureFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "malformed":
				f.frame.Payload = []byte{1}
			case "nonfinite":
				binary.LittleEndian.PutUint32(f.frame.Payload, math.Float32bits(float32(math.NaN())))
			case "subscribe":
				f.subscribeErr = errors.New("acknowledgement lost")
			case "cancelled":
				f.frame = nil
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			if _, err := readSensorTemperature(ctx, f); err == nil {
				t.Fatal("invalid read succeeded")
			}
			if !f.unsubscribed || f.listener != nil {
				t.Fatal("failed read retained subscription or listener")
			}
		})
	}
}
func TestSensorTemperatureRequiresExactLiveDeviceSession(t *testing.T) {
	b := New()
	b.lite = temperatureFixture()
	b.session = 7
	for _, session := range []uint64{0, 6} {
		if _, err := b.SensorTemperature(session); err == nil {
			t.Fatal("accepted missing/stale session")
		}
	}
	b.Shutdown()
}

func TestSensorTemperatureLostUnsubscribeClosesProducerOwner(t *testing.T) {
	f := temperatureFixture()
	f.unsubscribeErr = errors.New("stop acknowledgement lost")
	if _, err := readSensorTemperature(context.Background(), f); err == nil {
		t.Fatal("uncertain subscription reported as retired")
	}
	select {
	case <-f.Done():
	default:
		t.Fatal("uncertain source producer retained its client")
	}
	if f.listener != nil {
		t.Fatal("uncertain stop retained a data listener")
	}
}

// Simulate a slow mesh request which only returns once its transport closes.
// Foreground cancellation must not wait for its longer control timeout.
type blockedTemperatureManifest struct{ *temperatureLite }

func (f *blockedTemperatureManifest) GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error) {
	<-f.Done()
	return nil, errors.New("transport closed")
}
func TestSensorTemperatureCancellationInterruptsBlockedControlRequest(t *testing.T) {
	f := &blockedTemperatureManifest{temperatureFixture()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := readSensorTemperature(ctx, f); finished <- err }()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled read remained blocked in the control request")
	}
	if f.unsubscribed || f.listener != nil {
		t.Fatal("cancelled manifest request created a subscription")
	}
}
