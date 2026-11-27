// Package cameraframe retrieves native camera bytes through an ordinary,
// service-scoped World socket. Retrieval does not publish a signed observation.
package cameraframe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/jpeg"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
)

const (
	Version             = 1
	MaxJPEGBytes        = 8 << 20
	MaxMetadataBytes    = 32 << 10
	MaxCalibrationBytes = 16 << 10
	MaxChunkBytes       = 48 << 10
	MaxDimension        = 8192
	MaxPixels           = 32 << 20
)

var ErrFrame = errors.New("invalid native camera frame")

// Receipt records the gateway's receipt on Linux CLOCK_BOOTTIME. Its bracket
// bounds the gateway clock read, not the sensor capture or network delay.
// GatewayWallUnixNS, when supplied, is an unverified local wall-clock reading.
type Receipt struct {
	BootID            string `json:"boot_id"`
	BeforeBootNS      int64  `json:"before_boot_ns"`
	ReceivedBootNS    int64  `json:"received_boot_ns"`
	AfterBootNS       int64  `json:"after_boot_ns"`
	GatewayWallUnixNS int64  `json:"gateway_wall_unix_ns,omitempty"`
}

// Metadata preserves the native source fields without mapping TimestampUS to
// UTC or simulation time. SourceSessionID identifies a host connection, not a
// sensor boot. CalibrationJSON contains exact, uninterpreted upstream
// CameraMetadata JSON bytes; it is not validated World calibration.
type Metadata struct {
	Version         int     `json:"version"`
	SourceID        string  `json:"source_id"`
	SourceSessionID string  `json:"source_session_id"`
	Transport       string  `json:"transport"`
	ChannelID       uint32  `json:"channel_id"`
	FrameSequence   uint32  `json:"frame_sequence"`
	TimestampUS     uint64  `json:"timestamp_us"`
	TimestampBasis  string  `json:"timestamp_basis"`
	Flags           uint32  `json:"flags"`
	Receipt         Receipt `json:"receipt"`
	Codec           string  `json:"codec"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	JPEGSize        int64   `json:"jpeg_size"`
	JPEGSHA256      string  `json:"jpeg_sha256"`
	CalibrationJSON []byte  `json:"calibration_json,omitempty"`
}

type Frame struct {
	Metadata Metadata `json:"metadata"`
	JPEG     []byte   `json:"jpeg"`
}

// NewFrame owns its buffers and computes the size and digest from the original
// JPEG. All source, receipt and calibration fields must be supplied explicitly.
func NewFrame(m Metadata, jpegBytes []byte) (Frame, error) {
	if len(jpegBytes) == 0 || len(jpegBytes) > MaxJPEGBytes {
		return Frame{}, ErrFrame
	}
	m.JPEGSize = int64(len(jpegBytes))
	sum := sha256.Sum256(jpegBytes)
	m.JPEGSHA256 = hex.EncodeToString(sum[:])
	f := Frame{Metadata: m, JPEG: jpegBytes}
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	return f.Clone(), nil
}

func (f Frame) Clone() Frame {
	f.JPEG = bytes.Clone(f.JPEG)
	f.Metadata.CalibrationJSON = bytes.Clone(f.Metadata.CalibrationJSON)
	return f
}

func (m Metadata) Validate() error {
	if m.Version != Version || !token(m.SourceID, 256) || !token(m.SourceSessionID, 128) || m.TimestampBasis != "unknown" || m.Codec != "mjpeg" {
		return ErrFrame
	}
	switch m.Transport {
	case "tcp", "grpc", "wendycom":
	default:
		return ErrFrame
	}
	r := m.Receipt
	if !token(r.BootID, 128) || r.BeforeBootNS < 0 || r.AfterBootNS < r.BeforeBootNS || r.ReceivedBootNS != r.BeforeBootNS+(r.AfterBootNS-r.BeforeBootNS)/2 || r.GatewayWallUnixNS < 0 {
		return ErrFrame
	}
	if m.Width <= 0 || m.Height <= 0 || m.Width > MaxDimension || m.Height > MaxDimension || int64(m.Width)*int64(m.Height) > MaxPixels || m.JPEGSize <= 0 || m.JPEGSize > MaxJPEGBytes {
		return ErrFrame
	}
	digest, err := hex.DecodeString(m.JPEGSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != m.JPEGSHA256 {
		return ErrFrame
	}
	if len(m.CalibrationJSON) > 0 {
		if len(m.CalibrationJSON) > MaxCalibrationBytes {
			return ErrFrame
		}
		var object map[string]any
		if jws.DecodeStrict(m.CalibrationJSON, &object) != nil || len(object) == 0 {
			return ErrFrame
		}
	}
	return nil
}

func (f Frame) Validate() error {
	if err := f.Metadata.Validate(); err != nil {
		return err
	}
	if int64(len(f.JPEG)) != f.Metadata.JPEGSize {
		return ErrFrame
	}
	sum := sha256.Sum256(f.JPEG)
	if hex.EncodeToString(sum[:]) != f.Metadata.JPEGSHA256 {
		return ErrFrame
	}
	c, err := jpeg.DecodeConfig(bytes.NewReader(f.JPEG))
	if err != nil || c.Width != f.Metadata.Width || c.Height != f.Metadata.Height {
		return ErrFrame
	}
	return nil
}

func EncodeMetadata(m Metadata) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil || len(b) > MaxMetadataBytes {
		return nil, ErrFrame
	}
	return b, nil
}

func DecodeMetadata(b []byte) (Metadata, error) {
	var m Metadata
	if len(b) == 0 || len(b) > MaxMetadataBytes || jws.DecodeStrict(b, &m) != nil {
		return Metadata{}, ErrFrame
	}
	if !required(b, "version", "source_id", "source_session_id", "transport", "channel_id", "frame_sequence", "timestamp_us", "timestamp_basis", "flags", "receipt", "codec", "width", "height", "jpeg_size", "jpeg_sha256") {
		return Metadata{}, ErrFrame
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(b, &fields)
	if !required(fields["receipt"], "boot_id", "before_boot_ns", "received_boot_ns", "after_boot_ns") {
		return Metadata{}, ErrFrame
	}
	if err := m.Validate(); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

func required(b []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return false
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}

func token(s string, limit int) bool {
	if s == "" || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
