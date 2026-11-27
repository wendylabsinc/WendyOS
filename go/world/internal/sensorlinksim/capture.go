// Package sensorlinksim serves verified synthetic camera captures through the
// existing SensorLink protobuf framing. It does not impersonate enrolled devices.
package sensorlinksim

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

const MaxFrames = 1000
const MaxMetadataBytes = 8 << 20
const MaxMediaBytes = 64 << 20

type Frame struct {
	Sample sensing.Sample
	JPEG   []byte
}
type Capture struct {
	RunID, SensorID, PackageSHA256 string
	Width, Height, FPS             int
	Frames                         []Frame
}
type runReport struct {
	SchemaVersion string `json:"schema_version"`
	Synthetic     bool   `json:"synthetic"`
	PackageSHA256 string `json:"package_sha256"`
	Configuration struct {
		Frames     int      `json:"frames"`
		IntervalMS int      `json:"interval_ms"`
		Seed       uint64   `json:"seed"`
		RunID      string   `json:"run_id"`
		Alarm      bool     `json:"alarm"`
		Omit       []string `json:"omit"`
	} `json:"configuration"`
	Samples      int               `json:"samples"`
	BlobBytes    int64             `json:"blob_bytes"`
	Fidelity     map[string]string `json:"fidelity"`
	AssetLicense string            `json:"asset_license"`
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("capture input must be a regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("capture input changed type")
	}
	return f, nil
}
func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("capture input exceeds bound")
	}
	return b, nil
}
func hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// Load verifies the simulation report, selected PNG bytes and camera metadata,
// then encodes bounded JPEGs. All source files may be removed after this returns.
func Load(directory, sensor, runID string, quality int) (Capture, error) {
	var out Capture
	if directory == "" || sensor == "" || runID == "" || quality < 1 || quality > 100 {
		return out, errors.New("capture directory, sensor, run and JPEG quality 1..100 required")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return out, err
	}
	defer root.Close()
	raw, err := readRegular(root, "run.json", 65536)
	if err != nil {
		return out, err
	}
	var report runReport
	if err = authority.DecodeStrictLimit(raw, &report, 65536); err != nil {
		return out, err
	}
	var reportFields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &reportFields)
	for _, name := range []string{"schema_version", "synthetic", "package_sha256", "configuration", "samples", "blob_bytes", "fidelity", "asset_license"} {
		if value, exists := reportFields[name]; !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return out, fmt.Errorf("missing synthetic report field %s", name)
		}
	}
	var configuration map[string]json.RawMessage
	_ = json.Unmarshal(reportFields["configuration"], &configuration)
	for _, name := range []string{"frames", "interval_ms", "seed", "run_id", "alarm", "omit"} {
		if value, exists := configuration[name]; !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return out, fmt.Errorf("missing synthetic configuration field %s", name)
		}
	}
	cfg := report.Configuration
	if report.SchemaVersion != "wendy.sensor.run.v1" || !report.Synthetic || !sensing.DigestValid(report.PackageSHA256) || cfg.RunID != runID || cfg.Frames < 1 || cfg.Frames > MaxFrames || cfg.IntervalMS < 10 || cfg.IntervalMS > 1000 || cfg.IntervalMS%10 != 0 || cfg.Seed >= 1<<32 || report.Samples < 1 || report.Samples > MaxFrames*4 || report.BlobBytes < 0 || report.AssetLicense == "" {
		return out, errors.New("input is not a bounded declared synthetic sensor run")
	}
	st, err := root.Lstat("blobs")
	if err != nil {
		return out, err
	}
	if !st.IsDir() {
		return out, errors.New("blobs must be a directory")
	}
	blobRoot, err := root.OpenRoot("blobs")
	if err != nil {
		return out, err
	}
	defer blobRoot.Close()
	file, err := openRegular(root, "samples.jsonl")
	if err != nil {
		return out, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, MaxMetadataBytes+1))
	scanner.Buffer(make([]byte, 4096), sensing.MaxSampleBytes+1)
	out = Capture{RunID: runID, SensorID: sensor, PackageSHA256: report.PackageSHA256, FPS: 1000 / cfg.IntervalMS}
	totalMeta, totalMedia, count := 0, 0, 0
	var first *sensing.Sample
	for scanner.Scan() {
		line := scanner.Bytes()
		totalMeta += len(line) + 1
		count++
		if totalMeta > MaxMetadataBytes || count > MaxFrames*4 {
			return Capture{}, errors.New("capture metadata quota exceeded")
		}
		sample, err := sensing.Decode(line)
		if err != nil {
			return Capture{}, err
		}
		if sample.RunID != runID || sample.PackageSHA256 != report.PackageSHA256 || sample.Seed != cfg.Seed {
			return Capture{}, errors.New("sample differs from synthetic run report")
		}
		if sample.SensorID != sensor {
			continue
		}
		// A second-generation transport capture cannot silently become source footage.
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(line, &fields); err != nil {
			return Capture{}, err
		}
		if _, present := fields["sensorlink"]; present {
			return Capture{}, errors.New("input already carries SensorLink metadata")
		}
		if sample.Kind != "camera" || sample.Camera == nil || sample.Evidence == nil || sample.Evidence.ContentType != "image/png" || sample.Sequence != uint64(len(out.Frames)+1) || sample.SampleNS != 250000000+int64(len(out.Frames))*int64(cfg.IntervalMS)*1000000 {
			return Capture{}, errors.New("input must be an ordered synthetic PNG camera run")
		}
		if first == nil {
			clone := sample
			first = &clone
			out.Width = sample.Camera.Width
			out.Height = sample.Camera.Height
		} else {
			a, _ := json.Marshal(first.Camera)
			b, _ := json.Marshal(sample.Camera)
			if !bytes.Equal(a, b) || sample.SiteID != first.SiteID || sample.Frame != first.Frame || sample.ScenarioVersion != first.ScenarioVersion || sample.SensorModel != first.SensorModel || sample.CalibrationRevision != first.CalibrationRevision || sample.UncertaintyM != first.UncertaintyM {
				return Capture{}, errors.New("camera configuration changes within source run")
			}
		}
		media, err := readRegular(blobRoot, sample.Evidence.SHA256, sample.Evidence.Size)
		if err != nil {
			return Capture{}, err
		}
		if int64(len(media)) != sample.Evidence.Size || hash(media) != sample.Evidence.SHA256 {
			return Capture{}, errors.New("capture PNG digest or size differs")
		}
		dimensions, err := png.DecodeConfig(bytes.NewReader(media))
		if err != nil {
			return Capture{}, err
		}
		if dimensions.Width != out.Width || dimensions.Height != out.Height || dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > 1024 || dimensions.Height > 1024 {
			return Capture{}, errors.New("PNG dimensions differ from calibration")
		}
		pixels, err := png.Decode(bytes.NewReader(media))
		if err != nil {
			return Capture{}, err
		}
		var encoded bytes.Buffer
		if err = jpeg.Encode(&encoded, pixels, &jpeg.Options{Quality: quality}); err != nil {
			return Capture{}, err
		}
		if encoded.Len() == 0 || encoded.Len() > sensing.MaxEvidenceBytes {
			return Capture{}, errors.New("encoded JPEG exceeds frame bound")
		}
		totalMedia += encoded.Len()
		if totalMedia > MaxMediaBytes {
			return Capture{}, errors.New("encoded camera run exceeds memory quota")
		}
		out.Frames = append(out.Frames, Frame{Sample: sample, JPEG: append([]byte(nil), encoded.Bytes()...)})
	}
	if err = scanner.Err(); err != nil {
		return Capture{}, err
	}
	if count != report.Samples || len(out.Frames) != cfg.Frames {
		return Capture{}, fmt.Errorf("capture count differs from report: records=%d camera=%d", count, len(out.Frames))
	}
	if err = out.Validate(); err != nil {
		return Capture{}, err
	}
	return out, nil
}

func (c Capture) Validate() error {
	if c.RunID == "" || c.SensorID == "" || !sensing.DigestValid(c.PackageSHA256) || c.Width < 1 || c.Width > 1024 || c.Height < 1 || c.Height > 1024 || c.FPS < 1 || c.FPS > 100 || len(c.Frames) < 1 || len(c.Frames) > MaxFrames {
		return errors.New("invalid simulated capture")
	}
	total := 0
	previous := int64(-1)
	for _, frame := range c.Frames {
		s := frame.Sample
		if err := s.Validate(); err != nil {
			return err
		}
		if s.RunID != c.RunID || s.SensorID != c.SensorID || s.PackageSHA256 != c.PackageSHA256 || s.Kind != "camera" || s.Camera == nil || s.Camera.Width != c.Width || s.Camera.Height != c.Height || s.SampleNS%1000 != 0 || s.SampleNS <= previous || s.Sequence < 1 || s.Sequence > math.MaxUint32 {
			return errors.New("invalid camera frame lineage or clock")
		}
		previous = s.SampleNS
		if len(frame.JPEG) < 1 || len(frame.JPEG) > sensing.MaxEvidenceBytes {
			return errors.New("invalid encoded camera bytes")
		}
		decoded, err := jpeg.DecodeConfig(bytes.NewReader(frame.JPEG))
		if err != nil || decoded.Width != c.Width || decoded.Height != c.Height {
			return errors.New("encoded JPEG dimensions differ")
		}
		total += len(frame.JPEG)
		if total > MaxMediaBytes {
			return errors.New("camera run exceeds byte quota")
		}
	}
	return nil
}
