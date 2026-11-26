package ros2camera

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/jpeg"
	"math"
	"strings"
	"testing"
)

func floatImageSample(width, height, step uint32, bigEndian bool, values []float32) []byte {
	c := newCDRBuilder()
	c.header()
	c.u32(height)
	c.u32(width)
	c.str("32FC1")
	var order binary.ByteOrder = binary.LittleEndian
	if bigEndian {
		c.u8(1)
		order = binary.BigEndian
	} else {
		c.u8(0)
	}
	c.u32(step)
	data := make([]byte, int(step)*int(height))
	for y := 0; y < int(height); y++ {
		for x := 0; x < int(width) && x*4+4 <= int(step); x++ {
			order.PutUint32(data[y*int(step)+x*4:], math.Float32bits(values[y*int(width)+x]))
		}
	}
	c.bytes(data)
	return c.b
}

// IQ8's thermal app publishes a 32x24 ROS Image in floating-point Celsius.
// Both endian orders and padded rows must display without changing the source
// measurements. A generic 32FC1 image can also represent depth, so its preview
// uses relative intensity and makes no assumption about physical units.
func TestDecodeFloatImage(t *testing.T) {
	for _, bigEndian := range []bool{false, true} {
		values := make([]float32, 32*24)
		for i := range values {
			values[i] = 20 + float32(i%32)
		}
		sample := floatImageSample(32, 24, 32*4+8, bigEndian, values)
		original := bytes.Clone(sample)
		frame, err := DecodeFrame(TypeImage, sample)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Width != 32 || frame.Height != 24 || frame.Codec != CodecMJPEG {
			t.Fatalf("unexpected preview: %+v", frame)
		}
		img, err := jpeg.Decode(bytes.NewReader(frame.Data))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 24; y++ {
			low := color.GrayModel.Convert(img.At(0, y)).(color.Gray).Y
			high := color.GrayModel.Convert(img.At(31, y)).(color.Gray).Y
			if low > 5 || high < 250 {
				t.Fatalf("endian=%v row=%d: intensity range %d..%d", bigEndian, y, low, high)
			}
		}
		if !bytes.Equal(sample, original) {
			t.Fatal("preview modified source measurements")
		}
	}
}

func TestFloatImageSpecialValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []float32
		want   []uint8
	}{
		{"constant", []float32{25, 25, 25, 25}, []uint8{128, 128, 128, 128}},
		{"negative", []float32{-40, -20, 0, 40}, []uint8{0, 64, 128, 255}},
		{"nonfinite", []float32{float32(math.NaN()), float32(math.Inf(1)), -5, 5}, []uint8{0, 0, 0, 255}},
		{"extremes", []float32{-math.MaxFloat32, 0, math.MaxFloat32, float32(math.Inf(-1))}, []uint8{0, 128, 255, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Wide solid bands keep JPEG loss away from the sample centers.
			values := make([]float32, 32*8)
			for i := range values {
				values[i] = tc.values[(i%32)/8]
			}
			data, _, _, err := DecodeJPEG(TypeImage, floatImageSample(32, 8, 128, false, values))
			if err != nil {
				t.Fatal(err)
			}
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range tc.want {
				got := color.GrayModel.Convert(img.At(i*8+4, 4)).(color.Gray).Y
				if delta := int(got) - int(want); delta < -4 || delta > 4 {
					t.Errorf("band %d = %d, want %d", i, got, want)
				}
			}
		})
	}
}

func TestFloatImageRejectsInvalidFrames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sample  []byte
		message string
	}{
		{"short row", floatImageSample(2, 1, 4, false, []float32{1, 2}), "step"},
		{"no finite pixels", floatImageSample(2, 1, 8, false, []float32{float32(math.NaN()), float32(math.Inf(1))}), "finite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := DecodeJPEG(TypeImage, tc.sample); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error=%v, want %q", err, tc.message)
			}
		})
	}
	sample := floatImageSample(2, 1, 8, false, []float32{1, 2})
	if _, _, _, err := DecodeJPEG(TypeImage, sample[:len(sample)-1]); err == nil {
		t.Fatal("accepted truncated float image")
	}
}
