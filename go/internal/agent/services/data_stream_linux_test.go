//go:build linux

package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// End to end: an app writes to its recording sockets as it does today, with
// no change for live data, and a StreamData viewer receives each record.
func TestAppRecordingSocketToStreamData(t *testing.T) {
	sockets, capture := recordingSocketTestManager(t)
	dir, err := sockets.EnsureStreams("test.app", "worker", map[string]appconfig.RecordingStream{
		"markers": liveMarkersStream,
		"samples": {Mode: "durable", MediaType: "application/octet-stream"},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := os.MkdirTemp("", "wds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(agentDir) })
	path := filepath.Join(agentDir, "agent.sock")
	lis, err := localsocket.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	serveDataService(t, lis, capture)
	viewer := openLiveStream(t, dialDataService(t, "unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials())))

	if _, err = dialRecording(t, dir, "worker", "markers").Write([]byte("boxes")); err != nil {
		t.Fatal(err)
	}
	if r := recvLive(t, viewer); r.GetStreamId() != "apps/test.app/worker/markers" || r.GetSchema() != "wendy.data.v1.VideoMarkers" || string(r.GetPayload()) != "boxes" {
		t.Fatalf("lightweight record: %v", r)
	}

	// A durable envelope's sample time, not its arrival, is the record's time.
	durable := dialRecording(t, dir, "worker", "samples")
	b, _ := proto.Marshal(&recordingpb.Record{Id: "one", Payload: []byte{1, 2}, Timing: &recordingpb.SampleTiming{Clock: "UNIX", Count: 1, TimestampsNanos: []int64{1_700_000_000_000_000_000}}})
	if _, err = durable.Write(b); err != nil {
		t.Fatal(err)
	}
	if ack := receiveRecordingAck(t, durable); ack.State != recordingpb.Ack_COMMITTED {
		t.Fatalf("ack: %v", ack)
	}
	if r := recvLive(t, viewer); r.GetStreamId() != "apps/test.app/worker/samples" || r.GetTimestampUnixNanos() != 1_700_000_000_000_000_000 {
		t.Fatalf("durable record: %v", r)
	}
}

// The catalog follows an app's streams: listed while the app's task runs,
// active once it writes, and gone when it stops, though its sockets stay for
// the next start; and gone when they are released.
func TestAppRecordingStreamsInCatalog(t *testing.T) {
	sockets, capture := recordingSocketTestManager(t)
	catalog, err := capture.Live().Subscribe(live.Selection{StreamPrefixes: []string{live.CatalogPrefix}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Close)
	next := func(want agentpbv2.DataStreamInfo_State) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d, err := catalog.Next(ctx)
		if err != nil {
			t.Fatalf("no catalog record, want %v: %v", want, err)
		}
		var info agentpbv2.DataStreamInfo
		if err := proto.Unmarshal(d.Record.Payload, &info); err != nil {
			t.Fatal(err)
		}
		if info.GetStreamId() != "apps/test.app/worker/markers" || info.GetState() != want || info.GetSchema() != liveMarkersStream.Schema || info.GetPublisher() != "test.app" {
			t.Fatalf("got %v, want %v", &info, want)
		}
	}

	dir, err := sockets.EnsureStreams("test.app", "worker", map[string]appconfig.RecordingStream{"markers": liveMarkersStream})
	if err != nil {
		t.Fatal(err)
	}
	// Created, not started: nothing is listed.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if d, err := catalog.Next(ctx); err == nil {
		t.Fatalf("listed before the app started: %s", d.Record.StreamID)
	}
	sockets.AdvertiseStreams("test.app", "other")
	sockets.AdvertiseStreams("test.app", "worker")
	next(agentpbv2.DataStreamInfo_STATE_IDLE)
	if _, err = dialRecording(t, dir, "worker", "markers").Write([]byte("boxes")); err != nil {
		t.Fatal(err)
	}
	next(agentpbv2.DataStreamInfo_STATE_ACTIVE)
	sockets.UnadvertiseStreams("test.app", "worker")
	next(agentpbv2.DataStreamInfo_STATE_GONE)
	sockets.AdvertiseStreams("test.app", "worker")
	next(agentpbv2.DataStreamInfo_STATE_IDLE)
	sockets.Release("test.app", "worker")
	next(agentpbv2.DataStreamInfo_STATE_GONE)
}
