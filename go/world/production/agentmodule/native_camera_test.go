package agentmodule

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func nativeCameraBinding() NativeCameraBinding {
	return NativeCameraBinding{App: "com.example.sensors", Service: "telemetry-a", ComponentType: "wendy.sensor.image.v1", Stream: "front", SourceID: "sensorlink:asset:42:3"}
}

func nativeCameraConfig(c *Config) { c.NativeCameras = []NativeCameraBinding{nativeCameraBinding()} }

func TestNativeCameraBindingsUseExactExistingPublicationScope(t *testing.T) {
	policy := basePolicy()
	good := nativeCameraBinding()
	if err := validateNativeCameras(policy, nil); err != nil {
		t.Fatal("default camera configuration changed", err)
	}
	if err := validateNativeCameras(policy, []NativeCameraBinding{good}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"other-app", "other-service", "other-stream", "scalar", "path", "empty-source", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			binding := good
			switch mode {
			case "other-app":
				binding.App = "com.example.other"
			case "other-service":
				binding.Service = "telemetry-b"
			case "other-stream":
				binding.Stream = "other"
			case "scalar":
				binding.ComponentType, binding.Stream = "wendy.sensor.scalar.v1", "gas"
			case "path":
				binding.SourceID = "/dev/video96"
			case "empty-source":
				binding.SourceID = "sensorlink:"
			}
			bindings := []NativeCameraBinding{binding}
			if mode == "duplicate" {
				bindings = append(bindings, binding)
			}
			if err := validateNativeCameras(policy, bindings); err == nil {
				t.Fatal("camera mapping widened an existing publication grant")
			}
		})
	}

	m := &Manager{cfg: Config{NativeCameras: []NativeCameraBinding{good}}}
	scope := policy.scope(policy.Grants[0], 7)
	publication := serviceorigin.PublishScope{ComponentType: good.ComponentType, Stream: good.Stream}
	if got := m.nativeCameraSource(scope, publication); got != good.SourceID {
		t.Fatal("exact source did not resolve", got)
	}
	for _, mode := range []string{"app", "service", "grant", "stream"} {
		changed, requested := scope, publication
		switch mode {
		case "app":
			changed.App = "com.example.other"
		case "service":
			changed.Service = "telemetry-b"
		case "grant":
			changed.Publish = nil
		case "stream":
			requested.Stream = "other"
		}
		if got := m.nativeCameraSource(changed, requested); got != "" {
			t.Fatalf("%s resolved to %q", mode, got)
		}
	}
}

func TestNativeCameraProviderAndSourceInventory(t *testing.T) {
	m := &Manager{}
	provider := func(context.Context, string) (cameraframe.Frame, error) { return cameraframe.Frame{}, nil }
	if m.SetNativeCameraProvider(nil) == nil || m.SetNativeCameraProvider(provider) != nil || m.SetNativeCameraProvider(provider) == nil {
		t.Fatal("provider startup attachment was not exactly once")
	}
	closed := &Manager{closed: true}
	if closed.SetNativeCameraProvider(provider) == nil {
		t.Fatal("provider attached to a closed manager")
	}
	a := nativeCameraBinding()
	b := a
	b.SourceID = "sensorlink:asset:9:1"
	m.cfg.NativeCameras = []NativeCameraBinding{b, a, a}
	got := m.NativeCameraSourceIDs()
	if !reflect.DeepEqual(got, []string{a.SourceID, b.SourceID}) {
		t.Fatal("configured inventory is not sorted and unique", got)
	}
	got[0] = "changed"
	if m.NativeCameraSourceIDs()[0] != a.SourceID {
		t.Fatal("caller changed host source binding")
	}
}

func TestNativeCameraFinalizerChecksReaderWindowAfterPolicyIO(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "fresh"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			policy := basePolicy()
			clock := moduleTestClock()
			window, _ := clock.Now()
			scope := policy.scope(policy.Grants[0], 11)
			m := finalReaderManager(clock, policy, scope, "native-camera-test")
			m.bindings[bindingKey(scope.App, scope.Service)].authorityUntil = window.Latest.Add(10 * time.Millisecond)
			m.deps.PolicyCurrent = func(context.Context) error {
				if expired {
					finalReadAdvance(clock, 10*time.Millisecond)
				}
				return nil
			}
			chunk := &appv1.SensorFrameChunk{MetadataJson: []byte(`{"original":true}`), Data: []byte("original frame bytes")}
			out, err := m.finalizeResponse(context.Background(), scope, chunk)
			if expired {
				if err == nil || out != nil {
					t.Fatal("reader expired during policy I/O but camera bytes remained available")
				}
			} else if err != nil || out != chunk {
				t.Fatal("fresh reader lost original camera chunk", err)
			}
			if !bytes.Equal(chunk.Data, []byte("original frame bytes")) || !bytes.Equal(chunk.MetadataJson, []byte(`{"original":true}`)) || m.policyFailed.Load() {
				t.Fatal("reader window check rewrote data or changed still-current policy")
			}
		})
	}
}

func nativeCameraFrame(t *testing.T) cameraframe.Frame {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	var noise uint32 = 7
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			noise = noise*1664525 + 1013904223
			img.SetNRGBA(x, y, color.NRGBA{R: byte(noise), G: byte(noise >> 8), B: byte(noise >> 16), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() <= 48<<10 {
		t.Fatal("fixture must exercise more than one wire chunk")
	}
	frame, err := cameraframe.NewFrame(cameraframe.Metadata{
		Version: 1, SourceID: nativeCameraBinding().SourceID, SourceSessionID: "native-connection-1", Transport: "tcp",
		ChannelID: 3, FrameSequence: 71, TimestampUS: 123456789, TimestampBasis: "unknown", Flags: 5,
		Receipt: cameraframe.Receipt{BootID: "gateway-boot-1", BeforeBootNS: 1000, ReceivedBootNS: 1010, AfterBootNS: 1020},
		Codec:   "mjpeg", Width: 256, Height: 256,
	}, encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func nativeCameraReceive(t *testing.T, ctx context.Context, client appv1.WorldAppServiceClient, request *appv1.GetSensorFrameRequest) (cameraframe.Frame, int, error) {
	t.Helper()
	stream, err := client.GetSensorFrame(ctx, request)
	if err != nil {
		return cameraframe.Frame{}, 0, err
	}
	var raw, metadata []byte
	chunks := 0
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cameraframe.Frame{}, chunks, err
		}
		if chunk.Offset != int64(len(raw)) || len(chunk.Data) == 0 || len(chunk.Data) > 48<<10 {
			t.Fatal("invalid chunk boundary", chunk.Offset, len(chunk.Data))
		}
		if len(chunk.MetadataJson) != 0 {
			if metadata != nil && !bytes.Equal(metadata, chunk.MetadataJson) {
				t.Fatal("metadata changed within one snapshot")
			}
			metadata = bytes.Clone(chunk.MetadataJson)
		}
		raw = append(raw, chunk.Data...)
		chunks++
	}
	decoded, err := cameraframe.DecodeMetadata(metadata)
	if err != nil {
		return cameraframe.Frame{}, chunks, err
	}
	frame := cameraframe.Frame{Metadata: decoded, JPEG: raw}
	return frame, chunks, frame.Validate()
}

func TestNativeCameraLinuxOneSnapshotAcrossChunks(t *testing.T) {
	f := setupModule(t, nativeCameraConfig)
	want := nativeCameraFrame(t)
	var calls atomic.Int32
	if err := f.manager.SetNativeCameraProvider(func(_ context.Context, source string) (cameraframe.Frame, error) {
		calls.Add(1)
		if source != want.Metadata.SourceID {
			return cameraframe.Frame{}, ErrDenied
		}
		return want.Clone(), nil
	}); err != nil {
		t.Fatal(err)
	}
	client := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := &appv1.GetSensorFrameRequest{ComponentType: "wendy.sensor.image.v1", Stream: "front"}
	got, chunks, err := nativeCameraReceive(t, ctx, client, request)
	if err != nil || calls.Load() != 1 || chunks < 2 || !bytes.Equal(got.JPEG, want.JPEG) || !reflect.DeepEqual(got.Metadata, want.Metadata) {
		t.Fatal("native snapshot bytes or original metadata changed", err, calls.Load(), chunks)
	}
	for _, request := range []*appv1.GetSensorFrameRequest{
		{ComponentType: "wendy.sensor.image.v1", Stream: "other"},
		{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"},
	} {
		if _, _, err := nativeCameraReceive(t, ctx, client, request); err == nil {
			t.Fatal("unbound scope retrieved a native camera")
		}
	}
	other := moduleDial(t, f.mount(t, 1, 12).SocketPath)
	if _, _, err := nativeCameraReceive(t, ctx, other, request); err == nil || calls.Load() != 1 {
		t.Fatal("another service invoked the bound provider", err, calls.Load())
	}
}

func TestNativeCameraLinuxRefusesUnavailableOrChangedFrameRead(t *testing.T) {
	for _, mode := range []string{"unbound", "provider-absent", "wrong-source", "policy-change", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			configure := nativeCameraConfig
			if mode == "unbound" {
				configure = func(*Config) {}
			}
			f := setupModule(t, configure)
			frame := nativeCameraFrame(t)
			var calls atomic.Int32
			entered := make(chan struct{})
			if mode != "provider-absent" {
				if err := f.manager.SetNativeCameraProvider(func(ctx context.Context, _ string) (cameraframe.Frame, error) {
					calls.Add(1)
					switch mode {
					case "wrong-source":
						frame.Metadata.SourceID = "sensorlink:asset:77:3"
					case "policy-change":
						f.policyOK.Store(false)
					case "canceled":
						close(entered)
						<-ctx.Done()
						return cameraframe.Frame{}, ctx.Err()
					}
					return frame.Clone(), nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			client := moduleDial(t, f.mount(t, 0, 11).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if mode == "canceled" {
				go func() {
					select {
					case <-entered:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			_, chunks, err := nativeCameraReceive(t, ctx, client, &appv1.GetSensorFrameRequest{ComponentType: "wendy.sensor.image.v1", Stream: "front"})
			wantCalls := int32(1)
			if mode == "unbound" || mode == "provider-absent" {
				wantCalls = 0
			}
			if err == nil || chunks != 0 || calls.Load() != wantCalls {
				t.Fatal("refused camera read delivered data or called wrong provider", err, chunks, calls.Load())
			}
		})
	}
}
