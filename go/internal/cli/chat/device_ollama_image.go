package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
)

const ollamaSceneImageEdge = 640

// ollamaSceneImage makes a bounded wire-only preview for native Ollama chat.
// Original tool evidence and calibrated object-finding requests are unchanged.
func ollamaSceneImage(ctx context.Context, attachment Image) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := validateImages([]Image{attachment}); err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.Strict().DecodeString(attachment.Data)
	if err != nil {
		return "", err
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	edge := max(width, height)
	if edge <= ollamaSceneImageEdge {
		return attachment.Data, nil
	}
	width = max(1, int((int64(width)*ollamaSceneImageEdge+int64(edge)/2)/int64(edge)))
	height = max(1, int((int64(height)*ollamaSceneImageEdge+int64(edge)/2)/int64(edge)))
	preview := image.NewRGBA(image.Rect(0, 0, width, height))
	// Average source pixel areas while reducing resolution. This avoids
	// aliasing fine scene details without adding a resampling dependency.
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		y0, y1 := int(int64(y)*int64(bounds.Dy())/int64(height)), int(int64(y+1)*int64(bounds.Dy())/int64(height))
		for x := 0; x < width; x++ {
			x0, x1 := int(int64(x)*int64(bounds.Dx())/int64(width)), int(int64(x+1)*int64(bounds.Dx())/int64(width))
			var red, green, blue, count uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, b, a := source.At(bounds.Min.X+sx, bounds.Min.Y+sy).RGBA()
					// RGBA is alpha-premultiplied. Flatten transparent PNGs
					// onto white before producing an opaque JPEG preview.
					red += uint64(r + 0xffff - a)
					green += uint64(g + 0xffff - a)
					blue += uint64(b + 0xffff - a)
					count++
				}
			}
			preview.SetRGBA(x, y, color.RGBA{R: uint8(red / count / 257), G: uint8(green / count / 257), B: uint8(blue / count / 257), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, preview, &jpeg.Options{Quality: 80}); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes()), nil
}
