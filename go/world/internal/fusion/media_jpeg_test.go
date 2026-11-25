package fusion

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

func TestJPEGCameraEvidenceUsesBoundDimensionsAndDeclaredType(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			im.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var jpg, pngBytes bytes.Buffer
	if err := jpeg.Encode(&jpg, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&pngBytes, im); err != nil {
		t.Fatal(err)
	}
	camera := sensing.Camera{Width: 32, Height: 24}
	for _, test := range []struct {
		data []byte
		kind string
	}{{jpg.Bytes(), "image/jpeg"}, {pngBytes.Bytes(), "image/png"}} {
		fraction, err := RedFractionWithType(test.data, camera, test.kind)
		if err != nil || fraction < .99 {
			t.Fatal(fraction, err)
		}
		bad := camera
		bad.Width++
		if _, err = RedFractionWithType(test.data, bad, test.kind); err == nil {
			t.Fatal("wrong dimensions accepted")
		}
	}
	if _, err := RedFractionWithType(jpg.Bytes(), camera, "image/png"); err == nil {
		t.Fatal("JPEG laundered as PNG")
	}
	if _, err := RedFractionWithType(pngBytes.Bytes(), camera, "image/jpeg"); err == nil {
		t.Fatal("PNG laundered as JPEG")
	}
	if _, err := RedFractionWithType(jpg.Bytes()[:len(jpg.Bytes())-50], camera, "image/jpeg"); err == nil {
		t.Fatal("truncated JPEG accepted")
	}
}
