package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type liveDataClient struct {
	agentpbv2.DataServiceClient
	request *agentpbv2.StreamDataRequest
	stream  *liveDataClientStream
	err     error
}

func (c *liveDataClient) StreamData(_ context.Context, r *agentpbv2.StreamDataRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpbv2.DataRecord], error) {
	c.request = r
	return c.stream, c.err
}

type liveDataClientStream struct {
	grpc.ClientStream
	records []*agentpbv2.DataRecord
	end     error
	header  metadata.MD
}

func (s *liveDataClientStream) Header() (metadata.MD, error) { return s.header, nil }
func (s *liveDataClientStream) Recv() (*agentpbv2.DataRecord, error) {
	if len(s.records) > 0 {
		r := s.records[0]
		s.records = s.records[1:]
		return r, nil
	}
	if s.end != nil {
		return nil, s.end
	}
	return nil, io.EOF
}

func TestStreamLiveDataPrintsRecords(t *testing.T) {
	ts := time.Date(2026, 10, 25, 14, 3, 7, 250_000_000, time.Local).UnixNano()
	stream := &liveDataClientStream{header: metadata.MD{}, records: []*agentpbv2.DataRecord{
		{StreamId: "apps/demo/detector/markers", TimestampUnixNanos: ts, Schema: "wendy.data.v1.VideoMarkers", Payload: []byte{0x0a, 0x00, 0xff}},
		{StreamId: "apps/demo/log", TimestampUnixNanos: ts, Payload: []byte("  person\nentered  ")},
	}}
	client := &liveDataClient{stream: stream}
	var out, errOut bytes.Buffer
	req := &agentpbv2.StreamDataRequest{StreamIds: []string{"apps/demo/detector/markers", "apps/demo/log"}}
	if err := streamLiveData(context.Background(), client, req, &out, &errOut, false); err != nil {
		t.Fatal(err)
	}
	if len(client.request.GetStreamIds()) != 2 {
		t.Fatalf("selection not sent: %v", client.request)
	}
	want := "14:03:07.250  apps/demo/detector/markers  wendy.data.v1.VideoMarkers  3 B\n" +
		"14:03:07.250  apps/demo/log  -  18 B  person entered\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	if !strings.Contains(errOut.String(), "Ctrl-C") {
		t.Fatalf("no subscription notice on stderr: %q", errOut.String())
	}
}

func TestStreamLiveDataPrintsSkipped(t *testing.T) {
	stream := &liveDataClientStream{header: metadata.MD{}, records: []*agentpbv2.DataRecord{{StreamId: "apps/demo/log", Payload: []byte("hi"), Skipped: 3}}}
	var out bytes.Buffer
	if err := streamLiveData(t.Context(), &liveDataClient{stream: stream}, &agentpbv2.StreamDataRequest{}, &out, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "  apps/demo/log  -  2 B (3 skipped)  hi\n") {
		t.Fatalf("output: %q", out.String())
	}
}

// --json is one protobuf JSON object per line and nothing else on stdout, so
// a script can read records as they arrive.
func TestStreamLiveDataJSON(t *testing.T) {
	stream := &liveDataClientStream{header: metadata.MD{}, records: []*agentpbv2.DataRecord{
		{StreamId: "apps/demo/markers", TimestampUnixNanos: 42, Schema: "wendy.data.v1.VideoMarkers", Payload: []byte{1, 2}},
		{StreamId: "apps/demo/markers", TimestampUnixNanos: 43},
	}}
	var out, errOut bytes.Buffer
	if err := streamLiveData(context.Background(), &liveDataClient{stream: stream}, &agentpbv2.StreamDataRequest{}, &out, &errOut, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || errOut.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["stream_id"] != "apps/demo/markers" || first["timestamp_unix_nanos"] != "42" || first["payload"] != "AQI=" {
		t.Fatalf("record: %s", lines[0])
	}
}

func TestStreamLiveDataErrors(t *testing.T) {
	old := &liveDataClient{stream: &liveDataClientStream{end: status.Error(codes.Unimplemented, "unknown method StreamData")}}
	err := streamLiveData(context.Background(), old, &agentpbv2.StreamDataRequest{}, io.Discard, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "update the agent") {
		t.Fatalf("older agent: %v", err)
	}
	refused := &liveDataClient{stream: &liveDataClientStream{end: status.Error(codes.PermissionDenied, "streaming live data requires a client certificate")}}
	var errOut bytes.Buffer
	if err = streamLiveData(context.Background(), refused, &agentpbv2.StreamDataRequest{}, io.Discard, &errOut, false); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("refusal: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("a refused stream printed the subscription notice: %q", errOut.String())
	}
	// Ctrl-C cancels the context; that ends the command without an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := &liveDataClient{stream: &liveDataClientStream{header: metadata.MD{}, end: status.Error(codes.Canceled, "context canceled")}}
	if err = streamLiveData(ctx, cancelled, &agentpbv2.StreamDataRequest{}, io.Discard, io.Discard, false); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
}

func TestTextPreview(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    string
		ok      bool
	}{
		{`{"label":"person"}`, `{"label":"person"}`, true},
		{"a\tb\r\nc", "a b  c", true},
		{"\x00\x01", "", false},
		{"\xff\xfe", "", false},
		{"   ", "", false},
		{strings.Repeat("x", 100), strings.Repeat("x", 80) + "...", true},
	} {
		if got, ok := textPreview([]byte(tc.payload)); got != tc.want || ok != tc.ok {
			t.Errorf("textPreview(%q) = %q %v, want %q %v", tc.payload, got, ok, tc.want, tc.ok)
		}
	}
}

// A bad or busy --sse address fails at once, before the device connection,
// which here would fail on its own with an invalid device name.
func TestDataStreamSSEAddressCheckedBeforeConnecting(t *testing.T) {
	setTempConfig(t, &config.Config{})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldFlag := deviceFlag
	t.Cleanup(func() { deviceFlag = oldFlag })
	deviceFlag = "12345"
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for _, addr := range []string{"0.0.0.0:8090", busy.Addr().String()} {
		cmd := newDataStreamCmd()
		cmd.SetArgs([]string{"--sse", addr})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.ExecuteContext(context.Background()); err == nil || errors.Is(err, errInvalidDeviceName) {
			t.Errorf("--sse %s: got %v, want the address refused before connecting", addr, err)
		}
	}
}
