package usbthermal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"
)

func TestTC001MaxLayoutSizes(t *testing.T) {
	l := tc001Max
	checks := []struct {
		name      string
		got, want int
	}{
		{"row", l.RowBytes(), 512},
		{"picture", l.PictureBytes(), 98304},
		{"raw frame (256x384x2)", l.RawBytes(), 196608},
		{"metadata offset", l.MetadataOffset(), 196608},
		{"block (386 rows)", l.BlockBytes(), 197632},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d bytes, want %d", c.name, c.got, c.want)
		}
	}
}

func TestCountsToCelsius(t *testing.T) {
	cases := []struct {
		raw  uint16
		want float64
	}{
		{0, -273.15},
		{64, -272.15},       // one count is 1/64 K
		{17482, 0.0},        // 273.15 K * 64 = 17481.6, the nearest count
		{19028, 24.1625},    // the coldest pixel of the fixture block
		{19284, 28.1625},    // its hottest pixel
		{0x80 << 8, 238.85}, // a picture word read as a temperature: what a wrong offset decodes to
	}
	for _, c := range cases {
		if got := CountsToCelsius(c.raw); math.Abs(got-c.want) > 0.01 {
			t.Errorf("CountsToCelsius(%d) = %.4f, want %.4f", c.raw, got, c.want)
		}
	}
	if got := CountsToKelvin(64 * 300); got != 300 {
		t.Errorf("CountsToKelvin(19200) = %v, want 300", got)
	}
}

// TestRealBlockLayout pins the layout to the bytes the camera sent: where the
// picture, the temperatures, the metadata and the end-of-block row are, and
// that the temperatures describe the same scene as the picture.
func TestRealBlockLayout(t *testing.T) {
	block := realBlock(t)
	l := tc001Max
	picture := block[:l.PictureBytes()]
	temps := block[l.PictureBytes():l.RawBytes()]
	meta := block[l.MetadataOffset() : l.MetadataOffset()+l.RowBytes()]
	endRow := block[l.MetadataOffset()+l.RowBytes():]

	for i := 1; i < len(picture); i += 2 {
		if picture[i] != 0x80 {
			t.Fatalf("picture chroma byte %d = %#02x, want 0x80 everywhere (grey YUYV)", i, picture[i])
		}
	}
	if !bytes.Equal(endRow, NewParser(l).endRow) {
		t.Fatalf("last row is not 0xFF 0x00 x 256: % x...", endRow[:16])
	}

	raw := make([]uint16, l.Width*l.Height)
	for i := range raw {
		raw[i] = binary.LittleEndian.Uint16(temps[2*i:])
	}
	sorted := slices.Clone(raw)
	slices.Sort(sorted)
	if lo, hi, med := sorted[0], sorted[len(sorted)-1], sorted[len(sorted)/2]; lo != 19028 || hi != 19284 || med != 19052 {
		t.Fatalf("temperature counts min/median/max = %d/%d/%d, want 19028/19052/19284", lo, med, hi)
	}
	if c := CountsToCelsius(sorted[len(sorted)/2]); c < 24 || c > 25 {
		t.Fatalf("median %.2f degC is not the room it was measured in (24.5)", c)
	}

	// The picture is the camera's own rendering of the same temperatures:
	// the hottest pixel is the brightest, the coldest is (nearly) the darkest.
	hot := slices.Index(raw, sorted[len(sorted)-1])
	cold := slices.Index(raw, sorted[0])
	if hot != 6*l.Width+0 {
		t.Fatalf("hottest pixel at index %d, want row 6 column 0", hot)
	}
	maxLuma, minLuma := byte(0), byte(255)
	for i := 0; i < len(picture); i += 2 {
		maxLuma, minLuma = max(maxLuma, picture[i]), min(minLuma, picture[i])
	}
	if picture[2*hot] != maxLuma || maxLuma != 153 {
		t.Fatalf("hottest pixel luma %d, brightest %d; want both 153", picture[2*hot], maxLuma)
	}
	if picture[2*cold] > minLuma+2 {
		t.Fatalf("coldest pixel luma %d, darkest %d; want the coldest near the darkest", picture[2*cold], minLuma)
	}

	// Metadata: the first 64 words are zero, word 64 is the frame counter and
	// word 180 a second counter.
	for i := 0; i < 128; i++ {
		if meta[i] != 0 {
			t.Fatalf("metadata byte %d = %#02x, want the first 64 words zero", i, meta[i])
		}
	}
	if got := blockSequence(l, block); got != fixtureCounter {
		t.Fatalf("frame counter = %d, want %d", got, fixtureCounter)
	}
	if got := binary.LittleEndian.Uint16(meta[2*180:]); got != 1737 {
		t.Fatalf("second counter = %d, want 1737", got)
	}
	// The metadata row ends in 73 words of 0x00FF, the same bytes as the end
	// row: a search for the end row has to find the END of that run.
	tail := meta[2*183:]
	for i := 0; i < len(tail); i += 2 {
		if tail[i] != 0xFF || tail[i+1] != 0x00 {
			t.Fatalf("metadata tail byte %d = % x, want ff 00", 2*183+i, tail[i:i+2])
		}
	}
	if meta[2*182] == 0xFF {
		t.Fatal("the run of ff 00 must start at metadata word 183")
	}
}

func TestRawFrameTemperature(t *testing.T) {
	block := realBlock(t)
	f := RawFrame{Data: block[:tc001Max.RawBytes()], Width: 256, Height: 384, BytesPerLine: 512}
	got, ok := f.Temperature(0, 6)
	if !ok || got != 19284 {
		t.Fatalf("Temperature(0, 6) = %d, %v; want the hottest pixel, 19284", got, ok)
	}
	if got, ok := f.Temperature(0, 0); !ok || got != 19276 {
		t.Fatalf("Temperature(0, 0) = %d, %v; want 19276", got, ok)
	}
	for _, p := range [][2]int{{-1, 0}, {256, 0}, {0, 192}, {0, -1}} {
		if _, ok := f.Temperature(p[0], p[1]); ok {
			t.Errorf("Temperature(%d, %d) answered for a pixel outside the frame", p[0], p[1])
		}
	}
}

// TestCheckTemperatures: what a frame's temperature rows may hold before a raw
// subscriber is allowed to read them as degrees. A real scene passes, a real
// fire included; bytes that are not temperatures do not.
func TestCheckTemperatures(t *testing.T) {
	l := tc001Max
	room := realBlock(t)[:l.RawBytes()]
	set := func(data []byte, x, y int, v uint16) {
		binary.LittleEndian.PutUint16(data[l.PictureBytes()+y*l.RowBytes()+2*x:], v)
	}
	fill := func(c float64) []byte {
		d := bytes.Clone(room)
		for y := range l.Height {
			for x := range l.Width {
				set(d, x, y, celsiusToCounts(c))
			}
		}
		return d
	}
	pass := map[string][]byte{
		"the fixture's room (24.5 degC)": room,
		"a fire: 2,000 pixels at 550 degC": func() []byte {
			d := bytes.Clone(room)
			for y := 50; y < 90; y++ {
				for x := 100; x < 150; x++ {
					set(d, x, y, celsiusToCounts(550))
				}
			}
			return d
		}(),
		"dead pixels reading 0 and 0xFFFF": func() []byte {
			d := bytes.Clone(room)
			set(d, 0, 0, 0)
			set(d, 255, 191, 0xFFFF)
			return d
		}(),
		"a cold store at -30 degC":     fill(-30),
		"a hot plant room at 140 degC": fill(140),
	}
	for name, d := range pass {
		if err := rawFrameOf(d).CheckTemperatures(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	fail := map[string][]byte{
		"one word colder than -100 degC (a header's frame number)": func() []byte {
			d := bytes.Clone(room)
			set(d, 5, 126, 981)
			return d
		}(),
		"the picture rows read as temperatures": slices.Concat(room[:l.PictureBytes()], room[:l.PictureBytes()]),
		"the end-of-block row read as temperatures": slices.Concat(room[:l.PictureBytes()],
			bytes.Repeat([]byte{0xFF, 0x00}, l.PictureBytes()/2)),
		"no pixel with a reading": slices.Concat(room[:l.PictureBytes()], make([]byte, l.PictureBytes())),
		"median -50 degC":         fill(-50),
		"median 200 degC":         fill(200),
		"a short frame":           room[:l.RawBytes()-2],
	}
	for name, d := range fail {
		if err := rawFrameOf(d).CheckTemperatures(); !errors.Is(err, ErrNotTemperatures) {
			t.Errorf("%s: CheckTemperatures = %v, want ErrNotTemperatures", name, err)
		}
	}
}
