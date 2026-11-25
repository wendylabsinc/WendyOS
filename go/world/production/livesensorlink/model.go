// Package livesensorlink adapts a commissioned live simulation camera to the
// existing SensorLink transport and an ordinary World app socket. It supplies
// no physical sensor authentication, actuator access, or signing authority.
package livesensorlink

import (
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
)

const (
	MaxMetadataBytes = 64 << 10
	MaxCacheFrames   = 16
	MaxCaptureAge    = 5 * time.Second
	MaxResultBytes   = liveapp.MaxResultBytes
)

var (
	ErrConfig    = errors.New("live SensorLink configuration refused")
	ErrCapture   = errors.New("live SensorLink capture refused")
	ErrMetadata  = errors.New("live SensorLink frame metadata unavailable or inconsistent")
	ErrTransport = errors.New("live SensorLink transport unavailable")
)

type Commission struct {
	RunID         string         `json:"run_id"`
	BootID        string         `json:"boot_id"`
	PackageSHA256 string         `json:"package_sha256"`
	Site          string         `json:"site"`
	Frame         world.FrameRef `json:"frame"`
	SensorID      string         `json:"sensor_id"`
	AnchorUnixNS  int64          `json:"simulation_anchor_unix_ns"`
}

type Bounds struct {
	DurationMS       int64 `json:"duration_ms"`
	MaxFrames        int   `json:"max_frames"`
	RequestTimeoutMS int64 `json:"request_timeout_ms,omitempty"`
	TTLMS            int64 `json:"ttl_ms,omitempty"`
}

type DropChunk struct {
	FrameSequence uint32 `json:"frame_sequence"`
	ChunkSequence uint32 `json:"chunk_sequence"`
}

type SourceConfig struct {
	Commission                Commission `json:"commission"`
	Bounds                    Bounds     `json:"bounds"`
	IsolatedSimulationNetwork bool       `json:"isolated_simulation_network"`
	SceneURL                  string     `json:"scene_url"`
	SceneReadTokenFile        string     `json:"scene_read_token_file"`
	MetadataReadTokenFile     string     `json:"metadata_read_token_file"`
	TCPListen                 string     `json:"tcp_listen"`
	MetadataListen            string     `json:"metadata_listen"`
	ChannelID                 uint32     `json:"channel_id"`
	ChunkBytes                int        `json:"chunk_bytes,omitempty"`
	JPEGQuality               int        `json:"jpeg_quality,omitempty"`
	PollIntervalMS            int64      `json:"poll_interval_ms,omitempty"`
	Drop                      *DropChunk `json:"drop_chunk,omitempty"`
}

type BridgeConfig struct {
	Commission                Commission `json:"commission"`
	Bounds                    Bounds     `json:"bounds"`
	IsolatedSimulationNetwork bool       `json:"isolated_simulation_network"`
	TCPAddress                string     `json:"tcp_address"`
	MetadataURL               string     `json:"metadata_url"`
	MetadataReadTokenFile     string     `json:"metadata_read_token_file"`
	ChannelID                 uint32     `json:"channel_id"`
}

// Metadata contains the original capture bytes. It never substitutes the JPEG
// for the original PNG provenance; BindSensorLink creates that explicit link.
type Metadata struct {
	Version       int    `json:"version"`
	ChannelID     uint32 `json:"channel_id"`
	FrameSequence uint32 `json:"frame_sequence"`
	TimestampUS   uint64 `json:"timestamp_us"`
	Flags         uint32 `json:"flags"`
	JPEGSHA256    string `json:"jpeg_sha256"`
	JPEGSize      int64  `json:"jpeg_size"`
	CaptureJSON   []byte `json:"capture_json"`
}

type FrameReceipt struct {
	Metadata      Metadata `json:"metadata"`
	Chunks        int      `json:"chunks"`
	OmittedChunks int      `json:"omitted_chunks"`
}

type Result struct {
	Version       int               `json:"version"`
	Mode          string            `json:"mode"`
	RunID         string            `json:"run_id"`
	SensorID      string            `json:"sensor_id"`
	Status        string            `json:"status"`
	ErrorCode     string            `json:"error,omitempty"`
	FramesSent    int               `json:"frames_sent,omitempty"`
	ChunksSent    int               `json:"chunks_sent,omitempty"`
	ChunksOmitted int               `json:"chunks_omitted,omitempty"`
	DroppedFrames uint64            `json:"dropped_frames,omitempty"`
	Frames        []FrameReceipt    `json:"frames,omitempty"`
	Receipts      []liveapp.Receipt `json:"receipts,omitempty"`
}
