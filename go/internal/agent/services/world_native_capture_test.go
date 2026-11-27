package services

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
)

const worldNativeCameraID = "sensorlink:asset:8:4"

func worldNativeCameraFixture(t *testing.T) (*worldNativeCameraSource, []byte, *int) {
	t.Helper()
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	source, err := newWorldNativeCameraSource(context.Background(), []string{worldNativeCameraID},
		func(_ context.Context, id string) ([]byte, error) {
			calls++
			if id != worldNativeCameraID {
				t.Fatal("uncommissioned metadata lookup")
			}
			return []byte(`{"stable_id":"sensorlink:asset:8:4","revision":3}`), nil
		}, func() (cameraframe.Receipt, error) {
			return cameraframe.Receipt{BootID: "gateway-boot", BeforeBootNS: 100, ReceivedBootNS: 102, AfterBootNS: 104}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	source.session = func() (string, error) { count++; return "host-stream-" + strconv.Itoa(count), nil }
	return source, raw.Bytes(), &calls
}

func TestWorldNativeCameraOriginalMetadataAndOwnedSnapshot(t *testing.T) {
	source, jpegBytes, _ := worldNativeCameraFixture(t)
	observe, close := source.ObserveStream(context.Background(), mcusource.SensorPairing{SourceAssetID: 8, Transport: "wendycom"})
	defer close()
	original := bytes.Clone(jpegBytes)
	native := &sensorlink.SensorFrame{ChannelID: 4, Seq: 37, TsUs: 123456789, Flags: 5, Payload: jpegBytes}
	format := ros2camera.Frame{Data: jpegBytes, Codec: ros2camera.CodecMJPEG, Width: 4, Height: 4}
	observe(native, format)
	frame, err := source.ReadFrame(context.Background(), worldNativeCameraID)
	if err != nil {
		t.Fatal(err)
	}
	m := frame.Metadata
	if m.SourceID != worldNativeCameraID || m.SourceSessionID != "host-stream-1" || m.Transport != "wendycom" || m.ChannelID != 4 || m.FrameSequence != 37 || m.TimestampUS != 123456789 || m.TimestampBasis != "unknown" || m.Flags != 5 || m.Receipt.ReceivedBootNS != 102 || m.Receipt.GatewayWallUnixNS != 0 || !bytes.Equal(frame.JPEG, original) {
		t.Fatalf("native capture changed: %+v", m)
	}
	jpegBytes[0] ^= 1
	frame.JPEG[1] ^= 1
	frame.Metadata.CalibrationJSON[0] ^= 1
	again, err := source.ReadFrame(context.Background(), worldNativeCameraID)
	if err != nil || !bytes.Equal(again.JPEG, original) || string(again.Metadata.CalibrationJSON) != `{"stable_id":"sensorlink:asset:8:4","revision":3}` {
		t.Fatal("transport or caller mutated retained snapshot", err)
	}
	// A new native frame may wrap sequence/timestamp; these are raw fields,
	// never reconstructed from the host receipt clock or rewritten by the app.
	native.Seq, native.TsUs, native.Payload = 0, 0, original
	format.Data = original
	observe(native, format)
	next, err := source.ReadFrame(context.Background(), worldNativeCameraID)
	if err != nil || next.Metadata.FrameSequence != 0 || next.Metadata.TimestampUS != 0 || again.Metadata.FrameSequence != 37 {
		t.Fatal("original sequence or owned prior frame changed", err)
	}
}

func TestWorldNativeCameraSelectionSessionAndClose(t *testing.T) {
	source, raw, calls := worldNativeCameraFixture(t)
	pair := mcusource.SensorPairing{SourceAssetID: 8}
	if !source.WantsChannel(pair, 4) || source.WantsChannel(pair, 5) || source.WantsChannel(mcusource.SensorPairing{SourceAssetID: 9}, 4) {
		t.Fatal("observer-only subscription widened configured source")
	}
	old, oldClose := source.ObserveStream(context.Background(), pair)
	native := &sensorlink.SensorFrame{ChannelID: 5, Seq: 9, Payload: raw}
	format := ros2camera.Frame{Data: raw, Codec: ros2camera.CodecMJPEG, Width: 4, Height: 4}
	old(native, format)
	if *calls != 0 || len(source.latest) != 0 {
		t.Fatal("uncommissioned channel retained")
	}
	native.ChannelID = 4
	old(native, format)
	fresh, freshClose := source.ObserveStream(context.Background(), pair)
	fresh(native, format)
	oldClose()
	old(native, format)
	frame, err := source.ReadFrame(context.Background(), worldNativeCameraID)
	if err != nil || frame.Metadata.SourceSessionID != "host-stream-2" || frame.Metadata.Transport != "tcp" {
		t.Fatal("retired stream replaced or removed new frame", err)
	}
	freshClose()
	if _, err := source.ReadFrame(context.Background(), worldNativeCameraID); err == nil {
		t.Fatal("disconnected source remained available")
	}
	if _, err := source.ReadFrame(context.Background(), "sensorlink:asset:9:4"); err == nil {
		t.Fatal("uncommissioned source accepted")
	}
}

func TestWorldNativeCameraMissingCalibrationAndRefusal(t *testing.T) {
	source, raw, _ := worldNativeCameraFixture(t)
	source.metadata = func(context.Context, string) ([]byte, error) { return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	observe, close := source.ObserveStream(ctx, mcusource.SensorPairing{SourceAssetID: 8, Transport: "grpc"})
	defer close()
	native := &sensorlink.SensorFrame{ChannelID: 4, Payload: raw}
	format := ros2camera.Frame{Data: raw, Codec: ros2camera.CodecMJPEG, Width: 4, Height: 4}
	observe(native, format)
	frame, err := source.ReadFrame(context.Background(), worldNativeCameraID)
	if err != nil || frame.Metadata.CalibrationJSON != nil {
		t.Fatal("missing calibration was fabricated", err)
	}
	source.receipt = func() (cameraframe.Receipt, error) { return cameraframe.Receipt{}, errors.New("clock unavailable") }
	observe(native, format)
	if _, err := source.ReadFrame(context.Background(), worldNativeCameraID); err == nil {
		t.Fatal("failed replacement retained old frame")
	}
	cancel()
	observe(native, format)
	if _, err := source.ReadFrame(ctx, worldNativeCameraID); err == nil {
		t.Fatal("canceled read succeeded")
	}
	if !reflect.DeepEqual(raw, native.Payload) {
		t.Fatal("source bytes changed")
	}
	for _, ids := range [][]string{nil, {"/dev/video96"}, {worldNativeCameraID, worldNativeCameraID}, {"sensorlink:asset:8:04"}, {"sensorlink:" + strings.Repeat("x", 5) + ":bad"}} {
		if _, err := newWorldNativeCameraSource(context.Background(), ids, source.metadata, source.receipt); err == nil {
			t.Fatalf("invalid bindings accepted: %v", ids)
		}
	}
}
