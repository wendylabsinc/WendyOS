package livesensorlink

import (
	"context"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

func RunBridge(ctx context.Context, config BridgeConfig, api appv1.WorldAppServiceClient) (result Result, err error) {
	result = Result{Version: 1, Mode: "bridge", RunID: config.Commission.RunID, SensorID: config.Commission.SensorID, Status: "failed"}
	defer func() { resultError(&result, err) }()
	config, err = config.normalize()
	if err != nil || ctx == nil || api == nil {
		return result, ErrConfig
	}
	token, e := readToken(config.MetadataReadTokenFile)
	if e != nil {
		return result, e
	}
	metadata := newMetadataClient(config, token)
	defer metadata.http.CloseIdleConnections()
	publisher, e := liveapp.NewPublisher(api, liveapp.PublisherConfig{RequestTimeout: time.Duration(config.Bounds.RequestTimeoutMS) * time.Millisecond})
	if e != nil {
		return result, ErrConfig
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.Bounds.DurationMS)*time.Millisecond)
	defer cancel()
	conn, e := initialConnect(ctx, config.TCPAddress, time.Duration(config.Bounds.RequestTimeoutMS)*time.Millisecond)
	if e != nil {
		return result, ErrTransport
	}
	stream, e := sensorlink.Connect(ctx, conn, []uint32{config.ChannelID}, sensorlink.Options{HandshakeTimeout: time.Duration(config.Bounds.RequestTimeoutMS) * time.Millisecond, IdleTimeout: MaxCaptureAge, MaxFrameBytes: sensorlink.MaxFrameBytes})
	if e != nil {
		return result, ErrTransport
	}
	defer stream.Close()
	defer func() { result.DroppedFrames = stream.Dropped() }()
	var width, height uint32
	for _, d := range stream.Manifest.Sensors {
		if d.ChannelId == config.ChannelID {
			v := d.GetVideo()
			if d.Name != config.Commission.SensorID || v == nil || v.Codec != pb.VideoFormat_MJPEG || v.Width > 1024 || v.Height > 1024 {
				return result, ErrCapture
			}
			width, height = v.Width, v.Height
		}
	}
	for {
		frame, e := stream.Next(ctx)
		if e != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, ErrTransport
		}
		if frame.Seq == 0 || frame.Seq > uint32(config.Bounds.MaxFrames) {
			return result, ErrTransport
		}
		capture, e := metadata.fetch(ctx, *frame)
		if e != nil {
			return result, e
		}
		s, _ := capture.Sample()
		if s.Camera == nil || uint32(s.Camera.Width) != width || uint32(s.Camera.Height) != height {
			return result, ErrMetadata
		}
		bound, e := sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{ChannelID: frame.ChannelID, Seq: frame.Seq, TsUs: frame.TsUs, Flags: frame.Flags, Payload: frame.Payload})
		if e != nil {
			return result, ErrMetadata
		}
		request, e := sensorrecord.Publication(bound, time.Duration(config.Bounds.TTLMS)*time.Millisecond)
		if e != nil {
			return result, ErrCapture
		}
		receipt, e := publisher.Submit(ctx, request, frame.Payload)
		if e != nil {
			return result, e
		}
		result.Receipts = append(result.Receipts, receipt)
		// The transport retains only the newest pending frame. MaxFrames bounds
		// the finite source sequence, not the number of surviving publications.
		if frame.Seq == uint32(config.Bounds.MaxFrames) {
			result.Status = "complete"
			return result, nil
		}
	}
}

// Ordinary service dependencies order container starts, not listener readiness.
// Retry only refused/unavailable initial connections within five seconds. The
// first connected socket ends this loop permanently, even if its handshake fails.
func initialConnect(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	delay := 50 * time.Millisecond
	for {
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
		if conn != nil || err == nil {
			return conn, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 250*time.Millisecond)
	}
}
