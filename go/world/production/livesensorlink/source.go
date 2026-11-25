package livesensorlink

import (
	"bytes"
	"context"
	"image/jpeg"
	"image/png"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

type preparedFrame struct {
	metadata      Metadata
	jpeg          []byte
	width, height int
}

func prepare(snapshot livecapture.Snapshot, cfg SourceConfig, sequence uint32) (preparedFrame, error) {
	request, pngBytes, e := sensorrecord.FromSnapshot(snapshot, cfg.Commission.SensorID, time.Unix(0, cfg.Commission.AnchorUnixNS), time.Duration(cfg.Bounds.TTLMS)*time.Millisecond)
	if e != nil {
		return preparedFrame{}, ErrCapture
	}
	capture, e := sensorrecord.Decode(request.Payload)
	if e != nil || !commissioned(capture, cfg.Commission) || sequence == 0 {
		return preparedFrame{}, ErrCapture
	}
	// FromSnapshot validated the PNG digest and dimensions before decoding.
	im, e := png.Decode(bytes.NewReader(pngBytes))
	if e != nil {
		return preparedFrame{}, ErrCapture
	}
	var encoded bytes.Buffer
	if jpeg.Encode(&encoded, im, &jpeg.Options{Quality: cfg.JPEGQuality}) != nil || encoded.Len() > sensorlink.MaxFrameBytes {
		return preparedFrame{}, ErrCapture
	}
	data := encoded.Bytes()
	meta := Metadata{Version: 1, ChannelID: cfg.ChannelID, FrameSequence: sequence, TimestampUS: uint64(capture.CapturedUnixNS / 1000), Flags: sensorlink.FlagKeyframe, JPEGSHA256: digest(data), JPEGSize: int64(len(data)), CaptureJSON: bytes.Clone(request.Payload)}
	if _, e = checkMetadata(meta); e != nil {
		return preparedFrame{}, e
	}
	return preparedFrame{meta, data, im.Bounds().Dx(), im.Bounds().Dy()}, nil
}

// RunSource accepts one bounded session. Its original capture timestamps never
// change when a subscriber is slow, metadata is fetched later, or physics pauses.
func RunSource(ctx context.Context, config SourceConfig) (result Result, err error) {
	result = Result{Version: 1, Mode: "source", RunID: config.Commission.RunID, SensorID: config.Commission.SensorID, Status: "failed"}
	defer func() { resultError(&result, err) }()
	config, err = config.normalize()
	if err != nil || ctx == nil {
		return result, ErrConfig
	}
	sceneToken, e := readToken(config.SceneReadTokenFile)
	if e != nil {
		return result, e
	}
	metadataToken, e := readToken(config.MetadataReadTokenFile)
	if e != nil {
		return result, e
	}
	client, e := livecapture.New(livecapture.Config{URL: config.SceneURL, Token: sceneToken, RunID: config.Commission.RunID, Site: config.Commission.Site, Frame: config.Commission.Frame, MaxAge: 500 * time.Millisecond})
	if e != nil {
		return result, ErrConfig
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.Bounds.DurationMS)*time.Millisecond)
	defer cancel()
	streamListener, e := net.Listen("tcp", config.TCPListen)
	if e != nil {
		return result, ErrTransport
	}
	defer streamListener.Close()
	metadataListener, e := net.Listen("tcp", config.MetadataListen)
	if e != nil {
		return result, ErrTransport
	}
	defer metadataListener.Close()
	cache := newCache(metadataToken)
	server := &http.Server{Handler: cache, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	limited := newLimitedListener(metadataListener, 8)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(limited) }()
	defer func() { _ = server.Close(); <-serverDone }()
	stop := context.AfterFunc(ctx, func() { _ = streamListener.Close(); _ = server.Close() })
	defer stop()
	conn, e := streamListener.Accept()
	if e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrTransport
	}
	result, err = serveConn(ctx, conn, client, cache, config)
	if err != nil {
		return result, err
	}
	// Keep the final metadata available for its remaining bounded lifetime.
	// EOF is independent of metadata HTTP and cannot race away the last frame.
	timer := time.NewTimer(MaxCaptureAge)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	return result, nil
}

func serveConn(ctx context.Context, conn net.Conn, client *livecapture.Client, cache *metadataCache, cfg SourceConfig) (result Result, err error) {
	defer conn.Close()
	result = Result{Version: 1, Mode: "source", RunID: cfg.Commission.RunID, SensorID: cfg.Commission.SensorID, Status: "failed"}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	snapshot, e := client.Capture(ctx)
	if e != nil {
		return result, e
	}
	first, e := prepare(snapshot, cfg, 1)
	if e != nil {
		return result, e
	}
	timeout := time.Duration(cfg.Bounds.RequestTimeoutMS) * time.Millisecond
	if conn.SetDeadline(time.Now().Add(timeout)) != nil {
		return result, ErrTransport
	}
	manifest := &pb.SensorManifest{Sensors: []*pb.SensorDescriptor{{ChannelId: cfg.ChannelID, InputId: cfg.ChannelID, Name: cfg.Commission.SensorID, Format: &pb.SensorDescriptor_Video{Video: &pb.VideoFormat{Codec: pb.VideoFormat_MJPEG, Width: uint32(first.width), Height: uint32(first.height), Fps: uint32(max(1, 1000/cfg.PollIntervalMS))}}}}}
	if sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Manifest{Manifest: manifest}}) != nil {
		return result, ErrTransport
	}
	envelope, e := sensorlink.ReadMessage(conn)
	if e != nil {
		return result, ErrTransport
	}
	sub := envelope.GetSubscribe()
	if sub == nil || len(sub.ChannelId) != 1 || sub.ChannelId[0] != cfg.ChannelID {
		return result, ErrTransport
	}
	if conn.SetDeadline(time.Time{}) != nil {
		return result, ErrTransport
	}
	var native uint64
	var wall int64
	for sequence := uint32(1); sequence <= uint32(cfg.Bounds.MaxFrames); {
		current := first
		if sequence > 1 {
			timer := time.NewTimer(time.Duration(cfg.PollIntervalMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
			snapshot, e = client.Capture(ctx)
			if e != nil {
				return result, e
			}
			if snapshot.Sequence == native {
				continue
			}
			current, e = prepare(snapshot, cfg, sequence)
			if e != nil {
				return result, e
			}
		}
		if snapshot.Sequence <= native || snapshot.SampleUnixMS <= wall || current.width != first.width || current.height != first.height {
			return result, ErrCapture
		}
		if e = cache.put(current.metadata); e != nil {
			return result, e
		}
		chunks := (len(current.jpeg) + cfg.ChunkBytes - 1) / cfg.ChunkBytes
		if cfg.Drop != nil && cfg.Drop.FrameSequence == sequence && cfg.Drop.ChunkSequence >= uint32(chunks) {
			return result, ErrConfig
		}
		receipt := FrameReceipt{Metadata: current.metadata, Chunks: chunks}
		for offset, chunk := 0, uint32(0); offset < len(current.jpeg); chunk++ {
			end := min(offset+cfg.ChunkBytes, len(current.jpeg))
			if cfg.Drop != nil && cfg.Drop.FrameSequence == sequence && cfg.Drop.ChunkSequence == chunk {
				receipt.OmittedChunks++
				result.ChunksOmitted++
				offset = end
				continue
			}
			flags := uint32(0)
			if chunk == 0 {
				flags |= sensorlink.FlagKeyframe
			}
			if end == len(current.jpeg) {
				flags |= sensorlink.FlagLastChunk
			}
			data := &pb.SensorData{ChannelId: cfg.ChannelID, FrameSeq: sequence, ChunkSeq: chunk, TsUs: current.metadata.TimestampUS, Flags: flags, Payload: current.jpeg[offset:end]}
			if conn.SetWriteDeadline(time.Now().Add(timeout)) != nil || sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Data{Data: data}}) != nil {
				return result, ErrTransport
			}
			result.ChunksSent++
			offset = end
		}
		result.Frames = append(result.Frames, receipt)
		result.FramesSent++
		native, wall = snapshot.Sequence, snapshot.SampleUnixMS
		sequence++
	}
	result.Status = "complete"
	return result, nil
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}
type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func newLimitedListener(l net.Listener, n int) *limitedListener {
	return &limitedListener{Listener: l, slots: make(chan struct{}, n), done: make(chan struct{})}
}
func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, e := l.Listener.Accept()
	if e != nil {
		<-l.slots
		return nil, e
	}
	return &limitedConn{Conn: c, release: func() { <-l.slots }}, nil
}
func (l *limitedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}
func (c *limitedConn) Close() error { e := c.Conn.Close(); c.once.Do(c.release); return e }
