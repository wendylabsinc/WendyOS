package sensorrecord

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

// These checks validate media structure only. The agent does not import the
// fusion app's red-indicator, tone or hazard interpretation models.
func validateImage(data []byte, camera sensing.Camera, contentType string) error {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if (format != "png" && format != "jpeg") || contentType != "image/"+format || cfg.Width != camera.Width || cfg.Height != camera.Height || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1024 || cfg.Height > 1024 {
		return errors.New("camera bytes disagree with image format or calibration")
	}
	im, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if decodedFormat != format || im.Bounds().Dx() != cfg.Width || im.Bounds().Dy() != cfg.Height {
		return errors.New("decoded image disagrees with header")
	}
	return nil
}

func validateAudio(data []byte, audio sensing.Audio) error {
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return errors.New("invalid WAV container")
	}
	foundFormat, foundData, samples := false, false, 0
	for pos := 12; pos < len(data); {
		if len(data)-pos < 8 {
			return errors.New("truncated WAV chunk")
		}
		name := string(data[pos : pos+4])
		size := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if size > uint64(len(data)-pos) {
			return errors.New("WAV chunk exceeds data")
		}
		chunk := data[pos : pos+int(size)]
		switch name {
		case "fmt ":
			if foundFormat || len(chunk) != 16 {
				return errors.New("unsupported WAV format chunk")
			}
			foundFormat = true
			if binary.LittleEndian.Uint16(chunk[:2]) != 1 || int(binary.LittleEndian.Uint16(chunk[2:4])) != audio.Channels || int(binary.LittleEndian.Uint32(chunk[4:8])) != audio.SampleRateHz || binary.LittleEndian.Uint32(chunk[8:12]) != uint32(audio.SampleRateHz*2) || binary.LittleEndian.Uint16(chunk[12:14]) != 2 || binary.LittleEndian.Uint16(chunk[14:16]) != 16 || audio.Channels != 1 {
				return errors.New("WAV format disagrees with capture metadata")
			}
		case "data":
			if foundData || len(chunk) < 2 || len(chunk)%2 != 0 {
				return errors.New("duplicate or invalid PCM data")
			}
			foundData, samples = true, len(chunk)/2
		default:
			return errors.New("unsupported WAV chunk")
		}
		pos += int(size)
		if size%2 != 0 {
			if pos >= len(data) {
				return errors.New("missing WAV padding")
			}
			pos++
		}
	}
	if !foundFormat || !foundData || int64(samples)*1_000_000_000/int64(audio.SampleRateHz) != audio.DurationNS {
		return errors.New("WAV duration disagrees with captured audio interval")
	}
	return nil
}
