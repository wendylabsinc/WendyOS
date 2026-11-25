package fusion

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

// RedFraction measures the explicitly colored fixture indicator. It is not a
// person detector or general scene interpretation model.
func RedFraction(data []byte, camera sensing.Camera) (float64, error) {
	return RedFractionWithType(data, camera, "")
}

// RedFractionWithType additionally binds decoded image bytes to their evidence type.
func RedFractionWithType(data []byte, camera sensing.Camera, contentType string) (float64, error) {
	if len(data) < 1 || len(data) > sensing.MaxEvidenceBytes {
		return 0, errors.New("camera evidence exceeds bounds")
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil {
		return 0, e
	}
	if contentType != "" && ((format == "png" && contentType != "image/png") || (format == "jpeg" && contentType != "image/jpeg")) {
		return 0, errors.New("camera bytes disagree with evidence content type")
	}
	if (format != "png" && format != "jpeg") || cfg.Width != camera.Width || cfg.Height != camera.Height || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1024 || cfg.Height > 1024 {
		return 0, errors.New("camera image format or dimensions disagree with calibration")
	}
	im, actualFormat, e := image.Decode(bytes.NewReader(data))
	if e != nil {
		return 0, e
	}
	if actualFormat != format {
		return 0, errors.New("camera image format changed")
	}
	red := 0
	for y := im.Bounds().Min.Y; y < im.Bounds().Max.Y; y++ {
		for x := im.Bounds().Min.X; x < im.Bounds().Max.X; x++ {
			r, g, b, a := im.At(x, y).RGBA()
			if a == 65535 && r > 120*257 && float64(r) > 1.8*float64(g) && float64(r) > 1.8*float64(b) {
				red++
			}
		}
	}
	return float64(red) / float64(cfg.Width*cfg.Height), nil
}

type AudioFeatures struct {
	RMS          float64 `json:"rms"`
	ToneFraction float64 `json:"tone_fraction"`
}

// WAVFeatures reads bounded PCM16 mono WAV and estimates normalized energy and
// the fraction at the fixture's 880 Hz tone. No acoustic localization is implied.
func WAVFeatures(data []byte, audio sensing.Audio) (AudioFeatures, error) {
	var out AudioFeatures
	if len(data) < 44 || len(data) > sensing.MaxEvidenceBytes || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return out, errors.New("invalid WAV container")
	}
	var pcm []byte
	foundFmt := false
	for pos := 12; pos < len(data); {
		if len(data)-pos < 8 {
			return out, errors.New("truncated WAV chunk")
		}
		name := string(data[pos : pos+4])
		size := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if size > uint64(len(data)-pos) {
			return out, errors.New("WAV chunk exceeds data")
		}
		chunk := data[pos : pos+int(size)]
		switch name {
		case "fmt ":
			if foundFmt || len(chunk) != 16 {
				return out, errors.New("unsupported WAV format chunk")
			}
			foundFmt = true
			if binary.LittleEndian.Uint16(chunk[:2]) != 1 || int(binary.LittleEndian.Uint16(chunk[2:4])) != audio.Channels || int(binary.LittleEndian.Uint32(chunk[4:8])) != audio.SampleRateHz || binary.LittleEndian.Uint32(chunk[8:12]) != uint32(audio.SampleRateHz*2) || binary.LittleEndian.Uint16(chunk[12:14]) != 2 || binary.LittleEndian.Uint16(chunk[14:16]) != 16 || audio.Channels != 1 {
				return out, errors.New("WAV format disagrees with metadata")
			}
		case "data":
			if pcm != nil {
				return out, errors.New("duplicate WAV data")
			}
			pcm = chunk
		default:
			return out, errors.New("unsupported WAV chunk")
		}
		pos += int(size)
		if size%2 != 0 {
			if pos >= len(data) {
				return out, errors.New("missing WAV padding")
			}
			pos++
		}
	}
	if !foundFmt || len(pcm) < 2 || len(pcm)%2 != 0 || audio.SampleRateHz < 2000 || audio.SampleRateHz > 48000 {
		return out, errors.New("missing PCM samples")
	}
	count := len(pcm) / 2
	duration := int64(count) * 1_000_000_000 / int64(audio.SampleRateHz)
	if duration != audio.DurationNS {
		return out, errors.New("WAV duration disagrees with metadata")
	}
	energy, realPart, imaginary := 0.0, 0.0, 0.0
	for i := 0; i < count; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:2*i+2]))) / 32768
		energy += v * v
		angle := 2 * math.Pi * 880 * float64(i) / float64(audio.SampleRateHz)
		realPart += v * math.Cos(angle)
		imaginary += v * math.Sin(angle)
	}
	out.RMS = math.Sqrt(energy / float64(count))
	if energy > 0 {
		out.ToneFraction = math.Min(1, 2*(realPart*realPart+imaginary*imaginary)/(float64(count)*energy))
	}
	return out, nil
}
