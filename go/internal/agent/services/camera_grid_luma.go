package services

// Brightness of a JPEG camera, as a grid for viewers (camera_grid.go
// publishes it).
//
// Any camera that delivers whole JPEG frames, a SensorLink ESP32 camera for
// example, gets a grid of brightness from 0 to 100 %: its maximum is the
// brightest spot, its minimum the darkest, its mean how lit the scene is. The
// picture is averaged down to at most lumaGridWidth columns and lumaGridHeight
// rows, so a single noisy or saturated pixel cannot win the maximum or minimum
// the way it would at full resolution, and a viewer gets a few thousand values
// per frame rather than half a million. Ported from wendy-data-app's luma
// decoder.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"

	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

const (
	// lumaGridWidth and lumaGridHeight are the most columns and rows a
	// brightness grid has: an 800x600 SensorLink frame becomes 160x120. Bounding
	// both keeps the grid of a picture of any shape far below a record's 1 MiB.
	lumaGridWidth  = 160
	lumaGridHeight = 160
	// lumaFullScale is the count of a white cell. A count is the cell's mean
	// luma times 257, so luma 0..255 maps onto 0..65535 exactly.
	lumaFullScale = 65535
	// lumaMaxDimension bounds the picture a frame may declare before it is
	// decoded: the decoder allocates what the header says, and the header
	// comes from a paired board.
	lumaMaxDimension = 4096
)

// lumaGrid decodes one JPEG frame into a brightness grid of the camera's
// picture. Bytes after the image are ignored.
//
// The picture is split into cells of about block x block pixels, where block
// is the smallest whole number that keeps the grid at most lumaGridWidth wide
// and lumaGridHeight tall. Cell boundaries fall in proportion to the picture,
// so cell (col, row) covers exactly col/width to (col+1)/width of it across,
// and likewise down, as Grid.camera promises, even when block does not divide
// the picture. At 800x600 every cell is 5x5 pixels.
func lumaGrid(frame []byte, camera string) (*datapb.Grid, error) {
	if len(frame) < 4 || frame[0] != 0xFF || frame[1] != 0xD8 {
		return nil, fmt.Errorf("not a JPEG frame (%d bytes)", len(frame))
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	if cfg.Width > lumaMaxDimension || cfg.Height > lumaMaxDimension {
		return nil, fmt.Errorf("a %dx%d picture is larger than %d pixels a side", cfg.Width, cfg.Height, lumaMaxDimension)
	}
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	pw, ph := b.Dx(), b.Dy()
	block := max(1, (pw+lumaGridWidth-1)/lumaGridWidth, (ph+lumaGridHeight-1)/lumaGridHeight)
	w, h := pw/block, ph/block
	if w == 0 || h == 0 {
		return nil, errors.New("picture is smaller than one cell")
	}
	luma := lumaPlane(img)
	data := make([]byte, 2*w*h)
	for row := 0; row < h; row++ {
		y0, y1 := row*ph/h, (row+1)*ph/h
		for col := 0; col < w; col++ {
			x0, x1 := col*pw/w, (col+1)*pw/w
			sum := 0
			for y := y0; y < y1; y++ {
				for _, v := range luma[y*pw+x0 : y*pw+x1] {
					sum += int(v)
				}
			}
			n := (y1 - y0) * (x1 - x0)
			binary.LittleEndian.PutUint16(data[2*(row*w+col):], uint16((sum*257+n/2)/n))
		}
	}
	return &datapb.Grid{
		Width:        uint32(w),
		Height:       uint32(h),
		Unit:         "%",
		Data:         data,
		BytesPerLine: uint32(2 * w),
		Scale:        100.0 / lumaFullScale,
		Camera:       camera,
	}, nil
}

// lumaPlane returns one brightness byte per pixel, row-major. A colour JPEG
// decodes to YCbCr, whose Y plane is exactly that, and a grey one to Gray;
// anything else is converted.
func lumaPlane(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]uint8, w*h)
	switch p := img.(type) {
	case *image.YCbCr:
		for row := 0; row < h; row++ {
			start := p.YOffset(b.Min.X, b.Min.Y+row)
			copy(out[row*w:(row+1)*w], p.Y[start:start+w])
		}
	case *image.Gray:
		for row := 0; row < h; row++ {
			start := p.PixOffset(b.Min.X, b.Min.Y+row)
			copy(out[row*w:(row+1)*w], p.Pix[start:start+w])
		}
	default:
		for row := 0; row < h; row++ {
			for col := 0; col < w; col++ {
				out[row*w+col] = color.GrayModel.Convert(img.At(b.Min.X+col, b.Min.Y+row)).(color.Gray).Y
			}
		}
	}
	return out
}
