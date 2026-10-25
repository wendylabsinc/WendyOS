package services

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// greyJPEG encodes a w x h picture at 40 % grey with a white square and a
// black square, 40 pixels each, at the given corners. 800x600 is what a
// SensorLink ESP32 camera sends.
func greyJPEG(t *testing.T, w, h int, white, black image.Point, colour bool) []byte {
	t.Helper()
	var img image.Image
	set := func(x, y int, v uint8) {}
	if colour {
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		set = func(x, y int, v uint8) { rgba.Set(x, y, color.RGBA{v, v, v, 255}) }
		img = rgba
	} else {
		gray := image.NewGray(image.Rect(0, 0, w, h))
		set = func(x, y int, v uint8) { gray.SetGray(x, y, color.Gray{Y: v}) }
		img = gray
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 102) // 40 %
		}
	}
	fill := func(p image.Point, v uint8) {
		for y := p.Y; y < p.Y+40; y++ {
			for x := p.X; x < p.X+40; x++ {
				set(x, y, v)
			}
		}
	}
	fill(white, 255)
	fill(black, 0)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// brightnessAt reads the grid cell over picture pixel (x, y) in percent.
func brightnessAt(t *testing.T, g interface {
	GetWidth() uint32
	GetHeight() uint32
	GetBytesPerLine() uint32
	GetData() []byte
	GetScale() float64
	GetOffset() float64
}, pw, ph, x, y int) float64 {
	t.Helper()
	col, row := x*int(g.GetWidth())/pw, y*int(g.GetHeight())/ph
	count := binary.LittleEndian.Uint16(g.GetData()[row*int(g.GetBytesPerLine())+2*col:])
	return float64(count)*g.GetScale() + g.GetOffset()
}

// The cases of wendy-data-app's luma decoder: an 800x600 SensorLink frame
// averages down to 160x120 cells of 5x5 pixels, white reads 100 %, black 0 %
// and the 40 % grey 40 %. Trailing bytes after the image are ignored.
func TestLumaGridAveragesDownToBrightness(t *testing.T) {
	for _, colour := range []bool{true, false} {
		frame := greyJPEG(t, 800, 600, image.Pt(600, 100), image.Pt(100, 500), colour)
		g, err := lumaGrid(append(frame, make([]byte, 1000)...), "/dev/video96")
		if err != nil {
			t.Fatal(err)
		}
		if g.Width != 160 || g.Height != 120 || g.BytesPerLine != 320 || len(g.Data) != 2*160*120 || g.Unit != "%" || g.Camera != "/dev/video96" || g.Offset != 0 {
			t.Fatalf("grid %dx%d (%d B/line, %d B) %q %q offset %v", g.Width, g.Height, g.BytesPerLine, len(g.Data), g.Unit, g.Camera, g.Offset)
		}
		if full := float64(lumaFullScale) * g.Scale; math.Abs(full-100) > 1e-9 {
			t.Fatalf("full scale reads %v %%", full)
		}
		for _, c := range []struct {
			x, y int
			want float64
		}{{620, 120, 100}, {120, 520, 0}, {400, 300, 40}} {
			if got := brightnessAt(t, g, 800, 600, c.x, c.y); math.Abs(got-c.want) > 2 {
				t.Errorf("colour %v: brightness at (%d,%d) = %.1f %%, want about %.0f", colour, c.x, c.y, got, c.want)
			}
		}
	}
}

// When the cell size does not divide the picture, cells still split it in
// proportion: the last column ends at the picture's right edge, so a white
// band there lands in the last cells and nowhere else.
func TestLumaGridCellsSpanThePicture(t *testing.T) {
	// 1000 wide: cells of 7 pixels would leave 6 columns over. 142 cells
	// of 7 or 8 pixels cover all 1000.
	frame := greyJPEG(t, 1000, 600, image.Pt(960, 280), image.Pt(0, 0), true)
	g, err := lumaGrid(frame, "/dev/video97")
	if err != nil {
		t.Fatal(err)
	}
	if g.Width != 142 || g.Height != 85 {
		t.Fatalf("grid %dx%d, want 142x85", g.Width, g.Height)
	}
	// The white square spans x 960..999: the last cells, through the edge.
	if got := brightnessAt(t, g, 1000, 600, 999, 300); got < 90 {
		t.Errorf("rightmost cell at the white square reads %.1f %%", got)
	}
	if got := brightnessAt(t, g, 1000, 600, 940, 300); math.Abs(got-40) > 2 {
		t.Errorf("cell left of the white square reads %.1f %%", got)
	}
}

// A tall picture sets the cell size by its height, so its grid stays within
// 160 cells a side and fits a record: a 160x4096 frame used to make a grid of
// 160x4096 cells, 1.3 MB, which the record limit refused.
func TestLumaGridOfATallPictureFitsARecord(t *testing.T) {
	frame := greyJPEG(t, 160, 4096, image.Pt(10, 10), image.Pt(100, 100), false)
	g, err := lumaGrid(frame, "/dev/video96")
	if err != nil {
		t.Fatal(err)
	}
	// Cells of 26x26 pixels: ceil(4096/160).
	if g.Width != 6 || g.Height != 157 || len(g.Data) != 2*6*157 {
		t.Fatalf("grid %dx%d (%d B), want 6x157", g.Width, g.Height, len(g.Data))
	}
	if size := proto.Size(g); size > data.MaxRecordingPacket {
		t.Fatalf("grid is %d bytes, over the %d-byte record limit", size, data.MaxRecordingPacket)
	}
}

func TestLumaGridRefusesWhatIsNotAJPEG(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("hello world"), {0xFF, 0xD8, 0xFF, 0xE0, 1, 2}} {
		if _, err := lumaGrid(b, "/dev/video96"); err == nil {
			t.Errorf("decoded %q", b)
		}
	}
}

// A header that declares a huge picture is refused before the decoder
// allocates for it.
func TestLumaGridRefusesAHugeDeclaredPicture(t *testing.T) {
	frame := greyJPEG(t, 64, 48, image.Pt(0, 0), image.Pt(20, 0), false)
	// Rewrite the start-of-frame header to declare 60000x60000.
	sof := bytes.Index(frame, []byte{0xFF, 0xC0})
	if sof < 0 {
		t.Fatal("no baseline start-of-frame marker")
	}
	binary.BigEndian.PutUint16(frame[sof+5:], 60000)
	binary.BigEndian.PutUint16(frame[sof+7:], 60000)
	if _, err := lumaGrid(frame, "/dev/video96"); err == nil || !strings.Contains(err.Error(), "60000x60000") {
		t.Fatalf("a 60000x60000 picture: %v", err)
	}
}
