package cameraframe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
)

var ErrConfig = errors.New("invalid native camera client config")

// API must be the agent-owned World socket mounted for this service. The
// requested publication grant selects a host-configured source; there is no
// caller-selected device path or source identifier.
type API interface {
	GetSensorFrame(context.Context, *appv1.GetSensorFrameRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error)
}

type Config struct {
	ComponentType string
	Stream        string
	// Timeout bounds the entire stream, including EOF. Zero defaults to 5s.
	Timeout time.Duration
}

type Client struct {
	api API
	cfg Config
}

func New(api API, cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if api == nil || !token(cfg.ComponentType, 128) || !token(cfg.Stream, 128) || cfg.Timeout < time.Millisecond || cfg.Timeout > 30*time.Second {
		return nil, ErrConfig
	}
	return &Client{api: api, cfg: cfg}, nil
}

// Read makes one bounded request. A failed or incomplete stream returns no
// partial frame. Callers choose whether to request a new snapshot; Read never
// retries a native read and does not interpret the source's clock.
func (c *Client) Read(ctx context.Context) (Frame, error) {
	if c == nil || c.api == nil || ctx == nil {
		return Frame{}, ErrConfig
	}
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	stream, err := c.api.GetSensorFrame(ctx, &appv1.GetSensorFrameRequest{ComponentType: c.cfg.ComponentType, Stream: c.cfg.Stream}, grpc.MaxCallRecvMsgSize(MaxMetadataBytes+MaxChunkBytes+1024), grpc.MaxRetryRPCBufferSize(0), grpc.WaitForReady(false))
	if err != nil {
		return Frame{}, err
	}
	if stream == nil {
		return Frame{}, ErrFrame
	}
	var f Frame
	var metadata []byte
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return Frame{}, err
		}
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if count == 0 || int64(len(f.JPEG)) != f.Metadata.JPEGSize {
				return Frame{}, ErrFrame
			}
			if err := f.Validate(); err != nil {
				return Frame{}, err
			}
			if err := ctx.Err(); err != nil {
				return Frame{}, err
			}
			return f, nil
		}
		if err != nil {
			return Frame{}, err
		}
		if chunk == nil || len(chunk.ProtoReflect().GetUnknown()) != 0 || len(chunk.Data) == 0 || len(chunk.Data) > MaxChunkBytes || len(chunk.MetadataJson) > MaxMetadataBytes || chunk.Offset != int64(len(f.JPEG)) {
			return Frame{}, ErrFrame
		}
		if count == 0 {
			m, err := DecodeMetadata(chunk.MetadataJson)
			if err != nil {
				return Frame{}, err
			}
			f.Metadata = m
			metadata = bytes.Clone(chunk.MetadataJson)
			f.JPEG = make([]byte, 0, int(m.JPEGSize))
		} else if !bytes.Equal(metadata, chunk.MetadataJson) {
			return Frame{}, ErrFrame
		}
		// The producer emits full-sized chunks except the last. This bounds the
		// number of messages to ceil(MaxJPEGBytes/MaxChunkBytes), plus EOF.
		remaining := f.Metadata.JPEGSize - int64(len(f.JPEG))
		if int64(len(chunk.Data)) != min(int64(MaxChunkBytes), remaining) {
			return Frame{}, ErrFrame
		}
		f.JPEG = append(f.JPEG, chunk.Data...)
	}
}
