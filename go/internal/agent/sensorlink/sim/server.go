package sim

import (
	"context"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// Options configures a simulated sensor source.
type Options struct {
	Manifest      *sensorlinkpb.SensorManifest
	Frames        [][]byte // looped on every subscribed camera channel
	FrameInterval time.Duration
}

// Serve accepts sensorlink connections until ctx is cancelled or ln closes.
func Serve(ctx context.Context, ln net.Listener, opts Options) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-done:
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handleConn(ctx, conn, opts)
	}
}

func handleConn(ctx context.Context, conn net.Conn, opts Options) {
	defer conn.Close()
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Manifest{Manifest: opts.Manifest}}); err != nil {
		return
	}
	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		return
	}
	sub := env.GetSubscribe()
	if sub == nil || len(sub.ChannelId) == 0 {
		return
	}
	// Complete the subscribe handshake before closing an empty stream.
	// Otherwise the client races our close while writing its subscription.
	if len(opts.Frames) == 0 {
		return
	}
	interval := opts.FrameInterval
	if interval <= 0 {
		interval = 33 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seq uint32
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			payload := opts.Frames[int(seq)%len(opts.Frames)]
			for _, ch := range sub.ChannelId {
				frame := &sensorlinkpb.SensorFrame{ChannelId: ch, Seq: seq, TsUs: uint64(time.Now().UnixMicro()), Flags: 1, Payload: payload}
				if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Frame{Frame: frame}}); err != nil {
					return
				}
			}
			seq++
		}
	}
}
