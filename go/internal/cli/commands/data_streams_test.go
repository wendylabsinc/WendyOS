package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func catalogDataRecord(t *testing.T, info *agentpbv2.DataStreamInfo) *agentpbv2.DataRecord {
	t.Helper()
	payload, err := proto.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return &agentpbv2.DataRecord{StreamId: live.CatalogPrefix + info.GetStreamId(), TimestampUnixNanos: 1792936987000000000, Schema: live.CatalogSchema, Payload: payload}
}

func TestListLiveStreams(t *testing.T) {
	ts := time.Date(2026, 10, 25, 14, 3, 7, 250_000_000, time.Local).UnixNano()
	records := func() []*agentpbv2.DataRecord {
		return []*agentpbv2.DataRecord{
			catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/demo/log", MediaType: "text/plain", Publisher: "demo", State: agentpbv2.DataStreamInfo_STATE_IDLE}),
			catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/demo/detector/markers", Schema: "wendy.data.v1.VideoMarkers", Publisher: "demo", State: agentpbv2.DataStreamInfo_STATE_IDLE}),
			catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/old/markers", Publisher: "old", State: agentpbv2.DataStreamInfo_STATE_IDLE}),
			// Changes while listing: the newest entry wins, and a stream that
			// went away is left out.
			catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/demo/detector/markers", Schema: "wendy.data.v1.VideoMarkers", Publisher: "demo", State: agentpbv2.DataStreamInfo_STATE_ACTIVE, RateHz: 9.96, LastTimestampUnixNanos: ts}),
			catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/old/markers", Publisher: "old", State: agentpbv2.DataStreamInfo_STATE_GONE}),
		}
	}
	client := &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}, records: records()}}
	var out bytes.Buffer
	if err := listLiveStreams(context.Background(), client, &out, false, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := client.request; len(got.GetStreamPrefixes()) != 1 || got.GetStreamPrefixes()[0] != "_streams/" || !got.GetSendLatest() || len(got.GetStreamIds()) != 0 {
		t.Fatalf("request: %v", got)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := [][]string{
		{"STREAM", "STATE", "RATE", "LAST", "RECORD", "SCHEMA"},
		{"apps/demo/detector/markers", "active", "10.0/s", "14:03:07.250", "wendy.data.v1.VideoMarkers"},
		{"apps/demo/log", "idle", "-", "-", "-"},
	}
	if len(lines) != len(want) {
		t.Fatalf("output:\n%s", out.String())
	}
	for i, line := range lines {
		if got := strings.Fields(line); strings.Join(got, " ") != strings.Join(want[i], " ") {
			t.Fatalf("line %d: %q, want %q", i, line, want[i])
		}
	}

	out.Reset()
	client = &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}, records: records()}}
	if err := listLiveStreams(context.Background(), client, &out, true, time.Second); err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("json output:\n%s", out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["stream_id"] != "apps/demo/detector/markers" || first["state"] != "STATE_ACTIVE" || first["publisher"] != "demo" {
		t.Fatalf("json: %s", lines[0])
	}

	out.Reset()
	empty := &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}}}
	if err := listLiveStreams(context.Background(), empty, &out, false, time.Second); err != nil || out.String() != "No live data streams on this device.\n" {
		t.Fatalf("empty catalog: %q %v", out.String(), err)
	}
}

// openLiveStreamClient sends its records and then stays open, as a real
// catalog subscription does.
type openLiveStreamClient struct {
	agentpbv2.DataServiceClient
	records []*agentpbv2.DataRecord
}

type openLiveStreamClientStream struct {
	grpc.ClientStream
	ctx     context.Context
	records []*agentpbv2.DataRecord
}

func (c *openLiveStreamClient) StreamData(ctx context.Context, _ *agentpbv2.StreamDataRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpbv2.DataRecord], error) {
	return &openLiveStreamClientStream{ctx: ctx, records: c.records}, nil
}
func (s *openLiveStreamClientStream) Header() (metadata.MD, error) { return metadata.MD{}, nil }
func (s *openLiveStreamClientStream) Recv() (*agentpbv2.DataRecord, error) {
	if len(s.records) > 0 {
		r := s.records[0]
		s.records = s.records[1:]
		return r, nil
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

// The catalog stream never ends; the listing does once it has been quiet.
func TestListLiveStreamsSettles(t *testing.T) {
	client := &openLiveStreamClient{records: []*agentpbv2.DataRecord{
		catalogDataRecord(t, &agentpbv2.DataStreamInfo{StreamId: "apps/demo/log", State: agentpbv2.DataStreamInfo_STATE_IDLE}),
	}}
	var out bytes.Buffer
	start := time.Now()
	if err := listLiveStreams(context.Background(), client, &out, false, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > catalogMaxWait/2 {
		t.Fatalf("listing took %v", elapsed)
	}
	if !strings.Contains(out.String(), "apps/demo/log") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// An agent from before the catalog ignores the prefix and sends data records.
func TestListLiveStreamsOlderAgent(t *testing.T) {
	client := &openLiveStreamClient{records: []*agentpbv2.DataRecord{{StreamId: "apps/demo/log", Payload: []byte("x")}}}
	err := listLiveStreams(context.Background(), client, &bytes.Buffer{}, false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "update the agent") {
		t.Fatalf("older agent: %v", err)
	}
}
