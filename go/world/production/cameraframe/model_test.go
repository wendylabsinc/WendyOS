package cameraframe

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"
)

func testFrame(t *testing.T) Frame {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 512, 384))
	var n uint32 = 1
	for y := 0; y < 384; y++ {
		for x := 0; x < 512; x++ {
			n = n*1664525 + 1013904223
			im.SetRGBA(x, y, color.RGBA{uint8(n), uint8(n >> 8), uint8(n >> 16), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	m := Metadata{Version: Version, SourceID: "sensorlink:tcp:camera:7", SourceSessionID: "host-connection-1", Transport: "tcp", ChannelID: 7, FrameSequence: math.MaxUint32, TimestampUS: math.MaxUint64, TimestampBasis: "unknown", Flags: math.MaxUint32, Receipt: Receipt{BootID: "gateway-boot", BeforeBootNS: 100, ReceivedBootNS: 102, AfterBootNS: 105, GatewayWallUnixNS: 1794500000000000000}, Codec: "mjpeg", Width: 512, Height: 384, CalibrationJSON: []byte("{ \"stableId\": \"sensorlink:tcp:camera:7\", \"revision\": \"12\" }")}
	f, err := NewFrame(m, b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.JPEG) <= MaxChunkBytes {
		t.Fatal("fixture must exercise multiple chunks")
	}
	return f
}

func TestFramePreservesNativeFieldsAndOwnsBytes(t *testing.T) {
	f := testFrame(t)
	encoded, err := EncodeMetadata(f.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecodeMetadata(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if m.TimestampUS != math.MaxUint64 || m.FrameSequence != math.MaxUint32 || m.Flags != math.MaxUint32 || m.TimestampBasis != "unknown" || !bytes.Equal(m.CalibrationJSON, f.Metadata.CalibrationJSON) {
		t.Fatal("original native metadata changed")
	}
	copy, err := NewFrame(f.Metadata, f.JPEG)
	if err != nil {
		t.Fatal(err)
	}
	f.JPEG[20] ^= 1
	f.Metadata.CalibrationJSON[2] = 'x'
	if err := copy.Validate(); err != nil {
		t.Fatal("caller buffers changed owned frame", err)
	}
	cloned := copy.Clone()
	cloned.JPEG[20] ^= 1
	cloned.Metadata.CalibrationJSON[2] = 'x'
	if err := copy.Validate(); err != nil {
		t.Fatal("clone aliases original", err)
	}
	copy.Metadata.CalibrationJSON = nil
	copy.Metadata.FrameSequence = 0
	copy.Metadata.TimestampUS = 0
	copy.Metadata.Flags = 0
	copy.Metadata.ChannelID = 0
	if err := copy.Validate(); err != nil {
		t.Fatal("zero native fields or unknown calibration refused", err)
	}
}

func TestFrameRejectsInconsistentPacket(t *testing.T) {
	f := testFrame(t)
	cases := map[string]func(*Frame){
		"digest":                func(f *Frame) { f.JPEG[20] ^= 1 },
		"size":                  func(f *Frame) { f.Metadata.JPEGSize++ },
		"dimension":             func(f *Frame) { f.Metadata.Width++ },
		"sensor utc inference":  func(f *Frame) { f.Metadata.TimestampBasis = "utc" },
		"receipt order":         func(f *Frame) { f.Metadata.Receipt.AfterBootNS = 99 },
		"receipt midpoint":      func(f *Frame) { f.Metadata.Receipt.ReceivedBootNS++ },
		"unknown transport":     func(f *Frame) { f.Metadata.Transport = "fabricated" },
		"calibration duplicate": func(f *Frame) { f.Metadata.CalibrationJSON = []byte(`{"revision":1,"revision":2}`) },
		"calibration oversize":  func(f *Frame) { f.Metadata.CalibrationJSON = bytes.Repeat([]byte(" "), MaxCalibrationBytes+1) },
		"negative wall":         func(f *Frame) { f.Metadata.Receipt.GatewayWallUnixNS = -1 },
		"pixels":                func(f *Frame) { f.Metadata.Width = 8192; f.Metadata.Height = 8192 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			bad := f.Clone()
			change(&bad)
			if !errors.Is(bad.Validate(), ErrFrame) {
				t.Fatal("invalid packet accepted")
			}
		})
	}
	if _, err := NewFrame(f.Metadata, []byte("not a JPEG")); !errors.Is(err, ErrFrame) {
		t.Fatal("non-JPEG accepted")
	}
}

func TestMetadataStrictFieldsAndBounds(t *testing.T) {
	f := testFrame(t)
	b, _ := EncodeMetadata(f.Metadata)
	cases := map[string][]byte{
		"duplicate": append([]byte(`{"version":1,`), b[1:]...),
		"unknown":   append([]byte(`{"extra":1,`), b[1:]...),
		"alias":     bytes.Replace(b, []byte(`"frame_sequence"`), []byte(`"Frame_sequence"`), 1),
		"null":      bytes.Replace(b, []byte(`"flags":4294967295`), []byte(`"flags":null`), 1),
		"oversize":  bytes.Repeat([]byte(" "), MaxMetadataBytes+1),
		"trailing":  append(bytes.Clone(b), []byte("{}")...),
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	delete(fields, "timestamp_us")
	cases["missing"], _ = json.Marshal(fields)
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMetadata(bad); !errors.Is(err, ErrFrame) {
				t.Fatal("invalid metadata accepted", err)
			}
		})
	}
}
