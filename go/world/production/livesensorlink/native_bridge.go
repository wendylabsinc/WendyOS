package livesensorlink

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NativeFrameReceipt joins one acknowledged capture proof to the native
// metadata read for its exact JPEG. It does not independently verify that proof.
type NativeFrameReceipt struct {
	Metadata    cameraframe.Metadata `json:"metadata"`
	ProofSHA256 string               `json:"proof_sha256"`
}

type NativeBridgeResult struct {
	Result
	NativeFrames   []NativeFrameReceipt `json:"native_frames,omitempty"`
	DuplicatePolls uint64               `json:"duplicate_polls"`
}

type nativeFrameIdentity struct {
	source, session, transport, digest string
	channel, sequence, flags           uint32
	timestamp                          uint64
	size                               int64
	width, height                      int
}

func nativeIdentity(m cameraframe.Metadata) nativeFrameIdentity {
	return nativeFrameIdentity{m.SourceID, m.SourceSessionID, m.Transport, m.JPEGSHA256, m.ChannelID, m.FrameSequence, m.Flags, m.TimestampUS, m.JPEGSize, m.Width, m.Height}
}

func RunNativeBridge(ctx context.Context, config NativeBridgeConfig, api appv1.WorldAppServiceClient) (result NativeBridgeResult, err error) {
	result.Result = Result{Version: 1, Mode: "native-bridge", RunID: config.Commission.RunID, SensorID: config.Commission.SensorID, Status: "failed"}
	defer func() { resultError(&result.Result, err) }()
	config, err = config.normalize()
	if err != nil || ctx == nil || api == nil {
		return result, ErrConfig
	}
	token, e := readToken(config.MetadataReadTokenFile)
	if e != nil {
		return result, e
	}
	metadata := newMetadataClient(BridgeConfig{Commission: config.Commission, Bounds: config.Bounds, MetadataURL: config.MetadataURL}, token)
	defer metadata.http.CloseIdleConnections()
	reader, e := cameraframe.New(api, cameraframe.Config{ComponentType: sensorrecord.ComponentType, Stream: config.Commission.SensorID, Timeout: time.Duration(config.Bounds.RequestTimeoutMS) * time.Millisecond})
	if e != nil {
		return result, ErrConfig
	}
	publisher, e := liveapp.NewPublisher(api, liveapp.PublisherConfig{RequestTimeout: time.Duration(config.Bounds.RequestTimeoutMS) * time.Millisecond})
	if e != nil {
		return result, ErrConfig
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.Bounds.DurationMS)*time.Millisecond)
	defer cancel()
	interval := time.Duration(config.PollIntervalMS) * time.Millisecond
	frame, e := initialNativeFrame(ctx, reader, interval)
	if e != nil {
		return result, e
	}
	var last nativeFrameIdentity
	for {
		m := frame.Metadata
		if m.SourceID != config.SourceID || m.ChannelID != config.ChannelID || m.FrameSequence == 0 || m.FrameSequence > uint32(config.Bounds.MaxFrames) || m.Width > 1024 || m.Height > 1024 {
			return result, ErrMetadata
		}
		key := nativeIdentity(m)
		duplicate := false
		if last.session != "" {
			if key.session != last.session || key.transport != last.transport || key.sequence < last.sequence {
				return result, ErrMetadata
			}
			if key.sequence == last.sequence {
				if key != last {
					return result, ErrMetadata
				}
				duplicate = true
				result.DuplicatePolls++
			}
		}
		if !duplicate {
			wire := sensorlink.Frame{ChannelID: m.ChannelID, Seq: m.FrameSequence, TsUs: m.TimestampUS, Flags: m.Flags, Payload: frame.JPEG}
			capture, e := metadata.fetch(ctx, wire)
			if e != nil {
				return result, e
			}
			sample, e := capture.Sample()
			if e != nil || sample.Camera == nil || sample.Camera.Width != m.Width || sample.Camera.Height != m.Height {
				return result, ErrMetadata
			}
			bound, e := sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{ChannelID: wire.ChannelID, Seq: wire.Seq, TsUs: wire.TsUs, Flags: wire.Flags, Payload: wire.Payload})
			if e != nil {
				return result, ErrMetadata
			}
			request, e := sensorrecord.Publication(bound, time.Duration(config.Bounds.TTLMS)*time.Millisecond)
			if e != nil {
				return result, ErrCapture
			}
			receipt, e := publisher.Submit(ctx, request, frame.JPEG)
			if e != nil {
				return result, e
			}
			result.Receipts = append(result.Receipts, receipt)
			result.NativeFrames = append(result.NativeFrames, NativeFrameReceipt{Metadata: m, ProofSHA256: receipt.ProofSHA256})
			last = key
			if m.FrameSequence == uint32(config.Bounds.MaxFrames) {
				result.Status = "complete"
				return result, nil
			}
		}
		if e := nativePollWait(ctx, interval); e != nil {
			return result, e
		}
		frame, e = reader.Read(ctx)
		if e != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, ErrTransport
		}
	}
}

// Only no-first-frame Unavailable responses are polled for up to five seconds.
// After the first complete frame, read failures end the run. Publications and
// evidence uploads always retain liveapp's no-retry behavior.
func initialNativeFrame(ctx context.Context, reader *cameraframe.Client, interval time.Duration) (cameraframe.Frame, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		frame, err := reader.Read(ctx)
		if err == nil {
			return frame, nil
		}
		if ctx.Err() != nil {
			return cameraframe.Frame{}, ctx.Err()
		}
		if status.Code(err) != codes.Unavailable {
			return cameraframe.Frame{}, ErrTransport
		}
		if err = nativePollWait(ctx, interval); err != nil {
			return cameraframe.Frame{}, err
		}
	}
}

func nativePollWait(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
