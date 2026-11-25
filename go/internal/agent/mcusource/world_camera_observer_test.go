package mcusource_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

type worldObserverTransport struct {
	frames     chan *sensorlink.SensorFrame
	once       sync.Once
	streams    int
	manifest   *sensorlinkpb.SensorManifest
	subscribed []uint32
}

func (t *worldObserverTransport) Close() error { t.once.Do(func() { close(t.frames) }); return nil }
func (t *worldObserverTransport) FetchManifest(context.Context) (*sensorlinkpb.SensorManifest, error) {
	if t.manifest != nil {
		return t.manifest, nil
	}
	return camManifest(4, "native-camera"), nil
}
func (t *worldObserverTransport) Stream(_ context.Context, channels []uint32) (<-chan *sensorlink.SensorFrame, func() error, error) {
	t.streams++
	t.subscribed = append([]uint32(nil), channels...)
	return t.frames, t.Close, nil
}

type worldObserverWriter struct {
	frames int
	cancel context.CancelFunc
	err    error
}

func (w *worldObserverWriter) WriteFrame(ros2camera.Frame) error {
	w.frames++
	w.cancel()
	return w.err
}
func (w *worldObserverWriter) Close() error { return nil }

func TestWorldNativeCameraObserverPreservesNativeStream(t *testing.T) {
	for _, mode := range []string{"nil", "observed", "writer-error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			original := &sensorlink.SensorFrame{ChannelID: 4, Seq: 37, TsUs: 123456789, Flags: 1, Payload: []byte("original-jpeg")}
			transport := &worldObserverTransport{frames: make(chan *sensorlink.SensorFrame, 1)}
			transport.frames <- original
			writer := &worldObserverWriter{cancel: cancel}
			if mode == "writer-error" {
				writer.err = errors.New("test writer failure")
			}
			sup := mcusource.NewSupervisor(zap.NewNop(), &fakeLoopback{},
				func(mcusource.SensorPairing, string) (mcusource.SensorTransport, error) { return transport, nil },
				func(string) ros2camera.CameraWriter { return writer }, noAudioLoop{})
			pairing := mcusource.SensorPairing{SourceAssetID: 8, OrgID: 1, Transport: "tcp"}
			opened, observed, closed := 0, 0, 0
			if mode != "nil" {
				sup.SetCameraFrameObserver(func(_ context.Context, got mcusource.SensorPairing) (func(*sensorlink.SensorFrame, ros2camera.Frame), func()) {
					opened++
					if !reflect.DeepEqual(got, pairing) {
						t.Fatal("pairing identity changed")
					}
					return func(frame *sensorlink.SensorFrame, format ros2camera.Frame) {
						observed++
						if !reflect.DeepEqual(frame, original) || format.Width != 4 || format.Height != 4 || format.Codec != ros2camera.CodecMJPEG || writer.frames != 0 {
							t.Fatal("native fields changed or observation happened after writer")
						}
					}, func() { closed++ }
				}, nil)
			}
			_ = sup.RunPairing(ctx, pairing, "fixture")
			want := 1
			if mode == "nil" {
				want = 0
			}
			if opened != want || observed != want || closed != want || transport.streams != 1 || writer.frames != 1 {
				t.Fatalf("observer %d/%d/%d, streams %d, writes %d", opened, observed, closed, transport.streams, writer.frames)
			}
		})
	}
}

type worldUnavailableLoopback struct{ fakeLoopback }

func (*worldUnavailableLoopback) EnsureNode(context.Context, uint32, string) error {
	return errors.New("fixture has no v4l2loopback")
}

func TestWorldNativeCameraObserverWithoutLoopback(t *testing.T) {
	for _, mode := range []string{"configured", "unconfigured", "nil-wants", "nil"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			original := &sensorlink.SensorFrame{ChannelID: 4, Seq: 19, TsUs: 567890, Flags: 1, Payload: []byte("observer-only-jpeg")}
			transport := &worldObserverTransport{frames: make(chan *sensorlink.SensorFrame, 1)}
			transport.manifest = camManifest(4, "native-camera")
			transport.manifest.Sensors = append(transport.manifest.Sensors, camManifest(5, "unconfigured-camera").Sensors...)
			transport.frames <- original
			writers, observed, closed := 0, 0, 0
			sup := mcusource.NewSupervisor(zap.NewNop(), &worldUnavailableLoopback{},
				func(mcusource.SensorPairing, string) (mcusource.SensorTransport, error) { return transport, nil },
				func(string) ros2camera.CameraWriter { writers++; return &fakeWriter{} }, noAudioLoop{})
			pairing := mcusource.SensorPairing{SourceAssetID: 8, OrgID: 1, Transport: "tcp"}
			if mode != "nil" {
				var wants func(mcusource.SensorPairing, uint32) bool
				if mode != "nil-wants" {
					wants = func(got mcusource.SensorPairing, channel uint32) bool {
						return mode == "configured" && got.Key() == pairing.Key() && channel == 4
					}
				}
				sup.SetCameraFrameObserver(func(_ context.Context, got mcusource.SensorPairing) (func(*sensorlink.SensorFrame, ros2camera.Frame), func()) {
					if !reflect.DeepEqual(got, pairing) {
						t.Fatal("observer pairing differs")
					}
					return func(frame *sensorlink.SensorFrame, format ros2camera.Frame) {
						observed++
						if !reflect.DeepEqual(frame, original) || format.Width != 4 || format.Height != 4 || format.Codec != ros2camera.CodecMJPEG {
							t.Fatal("observer-only source fields changed")
						}
						cancel()
					}, func() { closed++ }
				}, wants)
			}
			_ = sup.RunPairing(ctx, pairing, "fixture")
			want := 0
			if mode == "configured" {
				want = 1
				if !reflect.DeepEqual(transport.subscribed, []uint32{4}) {
					t.Fatalf("subscribed outside configured channel: %v", transport.subscribed)
				}
			}
			if writers != 0 || transport.streams != want || observed != want || closed != want {
				t.Fatalf("writers %d, streams %d, observations %d, closes %d", writers, transport.streams, observed, closed)
			}
		})
	}
}
