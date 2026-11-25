package livesensorlink

import (
	"bytes"
	"context"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	captureapp "github.com/wendylabsinc/wendy/go/world/production/livecapture"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

// NativeSourceConfig serves the same simulation observations through the native
// SensorLink protocol. Dimensions make the manifest available before the first
// explicit scene step; every actual image must have these exact dimensions.
type NativeSourceConfig struct {
	SourceConfig
	Width  int          `json:"width"`
	Height int          `json:"height"`
	Ready  func() error `json:"-"`
}

func (c NativeSourceConfig) normalized() (NativeSourceConfig, error) {
	var err error
	c.SourceConfig, err = c.SourceConfig.normalize()
	if err != nil || c.Drop != nil || c.Width < 1 || c.Width > 4096 || c.Height < 1 || c.Height > 4096 {
		return c, ErrConfig
	}
	return c, nil
}

// NativeFrameSink receives owned exact wire bytes before they are sent. It may
// retain fixture evidence but must not modify the supplied original metadata.
type NativeFrameSink func(Metadata, []byte) error

// RunNativeSourceWithListener owns listener and closes it on every return. The
// caller supplies the native TLS listener. Empty subscriptions are manifest
// peeks; a subsequent exact channel subscription consumes the finite stream.
// It never changes capture time, sample sequence, calibration or simulation data.
func RunNativeSourceWithListener(ctx context.Context, config NativeSourceConfig, listener net.Listener, retain NativeFrameSink) (result Result, err error) {
	result = Result{Version: 1, Mode: "native-source", RunID: config.Commission.RunID, SensorID: config.Commission.SensorID, Status: "failed"}
	defer func() { resultError(&result, err) }()
	if listener == nil {
		return result, ErrConfig
	}
	defer listener.Close()
	config, err = config.normalized()
	if err != nil || ctx == nil {
		return result, ErrConfig
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	sceneToken, err := readToken(config.SceneReadTokenFile)
	if err != nil {
		return result, err
	}
	metadataToken, err := readToken(config.MetadataReadTokenFile)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.Bounds.DurationMS)*time.Millisecond)
	defer cancel()
	metadataListener, err := net.Listen("tcp", config.MetadataListen)
	if err != nil {
		return result, ErrTransport
	}
	cache := newCache(metadataToken)
	server := &http.Server{Handler: cache, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(newLimitedListener(metadataListener, 8)) }()
	defer func() { _ = server.Close(); <-serverDone }()
	if config.Ready != nil {
		if err = config.Ready(); err != nil {
			return result, err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close(); _ = server.Close() })
	defer stop()
	client := boundedHTTP(time.Duration(config.Bounds.RequestTimeoutMS) * time.Millisecond)
	defer client.CloseIdleConnections()
	// Native discovery uses one connection for FetchManifest and another for
	// Stream. Permit a bounded number of those read-only discovery connections.
	for connection := 0; connection < 16; connection++ {
		conn, e := listener.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, ErrTransport
		}
		streamed, e := nativeSourceConn(ctx, conn, config, client, sceneToken, cache, retain, &result)
		if e != nil {
			return result, e
		}
		if streamed {
			result.Status = "complete"
			return result, nil
		}
	}
	return result, ErrTransport
}

func nativeSourceConn(ctx context.Context, conn net.Conn, cfg NativeSourceConfig, client *http.Client, token string, cache *metadataCache, retain NativeFrameSink, result *Result) (bool, error) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	timeout := time.Duration(cfg.Bounds.RequestTimeoutMS) * time.Millisecond
	if conn.SetDeadline(time.Now().Add(timeout)) != nil {
		return false, ErrTransport
	}
	manifest := &pb.SensorManifest{Sensors: []*pb.SensorDescriptor{{ChannelId: cfg.ChannelID, InputId: cfg.ChannelID, Name: cfg.Commission.SensorID, Format: &pb.SensorDescriptor_Video{Video: &pb.VideoFormat{Codec: pb.VideoFormat_MJPEG, Width: uint32(cfg.Width), Height: uint32(cfg.Height), Fps: uint32(max(1, 1000/cfg.PollIntervalMS))}}}}}
	if sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Manifest{Manifest: manifest}}) != nil {
		return false, ErrTransport
	}
	envelope, err := sensorlink.ReadMessage(conn)
	if err != nil {
		return false, ErrTransport
	}
	sub := envelope.GetSubscribe()
	if sub == nil {
		return false, ErrTransport
	}
	if len(sub.ChannelId) == 0 {
		return false, nil
	}
	if len(sub.ChannelId) != 1 || sub.ChannelId[0] != cfg.ChannelID {
		return false, ErrTransport
	}
	if conn.SetDeadline(time.Time{}) != nil {
		return false, ErrTransport
	}
	var previous []byte
	var native uint64
	var sampleNS, wallNS int64
	for sequence := uint32(1); sequence <= uint32(cfg.Bounds.MaxFrames); {
		raw, err := nativeCaptureHTTP(ctx, client, cfg, token)
		if err != nil {
			return false, err
		}
		if len(raw) > 0 {
			capture, err := captureapp.Decode(raw, captureapp.Config{RunID: cfg.Commission.RunID, Site: cfg.Commission.Site, Frame: cfg.Commission.Frame, SensorID: cfg.Commission.SensorID, SimulationAnchorUnixNS: cfg.Commission.AnchorUnixNS})
			if err != nil || !commissioned(capture.Capture, cfg.Commission) {
				return false, ErrCapture
			}
			sample, _ := capture.Capture.Sample()
			if sample.Sequence < native || sample.Sequence == native && !bytes.Equal(raw, previous) {
				return false, ErrCapture
			}
			if sample.Sequence > native {
				if sample.SampleNS <= sampleNS || capture.Capture.CapturedUnixNS <= wallNS {
					return false, ErrCapture
				}
				frame, err := prepareNativeCapture(capture, cfg, sequence)
				if err != nil {
					return false, err
				}
				if err = cache.put(frame.metadata); err != nil {
					return false, err
				}
				if retain != nil {
					meta := frame.metadata
					meta.CaptureJSON = bytes.Clone(meta.CaptureJSON)
					if err = retain(meta, bytes.Clone(frame.jpeg)); err != nil {
						return false, err
					}
				}
				chunks := (len(frame.jpeg) + cfg.ChunkBytes - 1) / cfg.ChunkBytes
				for offset, chunk := 0, uint32(0); offset < len(frame.jpeg); chunk++ {
					end := min(offset+cfg.ChunkBytes, len(frame.jpeg))
					flags := uint32(0)
					if chunk == 0 {
						flags |= sensorlink.FlagKeyframe
					}
					if end == len(frame.jpeg) {
						flags |= sensorlink.FlagLastChunk
					}
					data := &pb.SensorData{ChannelId: cfg.ChannelID, FrameSeq: sequence, ChunkSeq: chunk, TsUs: frame.metadata.TimestampUS, Flags: flags, Payload: frame.jpeg[offset:end]}
					if conn.SetWriteDeadline(time.Now().Add(timeout)) != nil || sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Data{Data: data}}) != nil {
						return false, ErrTransport
					}
					result.ChunksSent++
					offset = end
				}
				result.Frames = append(result.Frames, FrameReceipt{Metadata: frame.metadata, Chunks: chunks})
				result.FramesSent++
				native, sampleNS, wallNS = sample.Sequence, sample.SampleNS, capture.Capture.CapturedUnixNS
				previous = bytes.Clone(raw)
				sequence++
				if sequence > uint32(cfg.Bounds.MaxFrames) {
					// Keep the native session and HTTP cache together until the final
					// original capture expires. Receipt never extends that deadline.
					delay := time.Until(time.Unix(0, wallNS).Add(MaxCaptureAge))
					if delay > 0 {
						if err = waitNativePoll(ctx, delay); err != nil {
							return false, err
						}
					}
					return true, nil
				}
			}
		}
		if err = waitNativePoll(ctx, time.Duration(cfg.PollIntervalMS)*time.Millisecond); err != nil {
			return false, err
		}
	}
	return true, nil
}

func nativeCaptureHTTP(ctx context.Context, client *http.Client, cfg NativeSourceConfig, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.SceneURL+"/v1/captures/"+cfg.Commission.SensorID, nil)
	if err != nil {
		return nil, ErrConfig
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, captureapp.MaxResponseBytes+1))
	if err != nil || len(raw) > captureapp.MaxResponseBytes {
		return nil, ErrCapture
	}
	if res.StatusCode == http.StatusNoContent && len(raw) == 0 {
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, ErrCapture
	}
	return raw, nil
}

func prepareNativeCapture(c captureapp.Capture, cfg NativeSourceConfig, sequence uint32) (preparedFrame, error) {
	if c.Capture.SensorLink != nil || c.Capture.CapturedUnixNS%1000 != 0 || sequence == 0 || !captureFresh(c.Capture, time.Now()) {
		return preparedFrame{}, ErrCapture
	}
	raw, err := sensorrecord.Encode(c.Capture)
	if err != nil {
		return preparedFrame{}, ErrCapture
	}
	im, err := png.Decode(bytes.NewReader(c.Media))
	if err != nil || im.Bounds().Dx() != cfg.Width || im.Bounds().Dy() != cfg.Height {
		return preparedFrame{}, ErrCapture
	}
	var encoded bytes.Buffer
	if jpeg.Encode(&encoded, im, &jpeg.Options{Quality: cfg.JPEGQuality}) != nil || encoded.Len() > sensorlink.MaxFrameBytes {
		return preparedFrame{}, ErrCapture
	}
	data := encoded.Bytes()
	meta := Metadata{Version: 1, ChannelID: cfg.ChannelID, FrameSequence: sequence, TimestampUS: uint64(c.Capture.CapturedUnixNS / 1000), Flags: sensorlink.FlagKeyframe, JPEGSHA256: digest(data), JPEGSize: int64(len(data)), CaptureJSON: raw}
	if _, err = checkMetadata(meta); err != nil {
		return preparedFrame{}, err
	}
	return preparedFrame{meta, data, cfg.Width, cfg.Height}, nil
}

func waitNativePoll(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
