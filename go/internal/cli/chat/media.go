package chat

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

const (
	maxImageBytes        = 4 * 1024 * 1024
	maxImagePixels       = 8 * 1024 * 1024
	maxImagesPerResult   = 4
	maxResultImageBytes  = 8 * 1024 * 1024
	maxHistoryImageBytes = 16 * 1024 * 1024
	maxHistoryImages     = 16
)

func validateImages(images []Image) ([]Image, error) {
	if len(images) > maxImagesPerResult {
		return nil, errors.New("tool returned more than 4 images; request fewer snapshots")
	}
	total := 0
	for _, attachment := range images {
		if attachment.MIMEType != "image/jpeg" && attachment.MIMEType != "image/png" {
			return nil, fmt.Errorf("unsupported image format %q; use JPEG or PNG", attachment.MIMEType)
		}
		if len(attachment.Data) > base64.StdEncoding.EncodedLen(maxImageBytes) {
			return nil, errors.New("image exceeds 4 MiB; request a smaller snapshot")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(attachment.Data)
		if err != nil {
			return nil, errors.New("tool returned invalid base64 image data")
		}
		total += len(data)
		if total > maxResultImageBytes {
			return nil, errors.New("tool images exceed 8 MiB; request fewer snapshots")
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, errors.New("tool returned an unreadable image")
		}
		if "image/"+format != attachment.MIMEType {
			return nil, errors.New("image content does not match its MIME type")
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImagePixels/cfg.Height {
			return nil, errors.New("image exceeds 8 megapixels; request a smaller snapshot")
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return nil, errors.New("tool returned a damaged image")
		}
	}
	return append([]Image(nil), images...), nil
}

// Retain recent visual evidence without allowing repeated captures to grow
// session memory indefinitely. Text metadata remains when an old image expires.
func (e *Engine) trimImageHistory() {
	total := 0
	count := 0
	for i := len(e.messages) - 1; i >= 0; i-- {
		m := &e.messages[i]
		size := 0
		for _, image := range m.Images {
			size += len(image.Data)
		}
		if total+size > maxHistoryImageBytes || count+len(m.Images) > maxHistoryImages {
			m.Images = nil
			m.Content += "\n[Earlier image attachment expired from context. Capture a fresh snapshot if visual details are needed.]"
		} else {
			total += size
			count += len(m.Images)
		}
	}
}
