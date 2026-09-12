package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// stubDataClient is a DataServiceClient that answers Download with an
// immediately exhausted stream and refuses everything else. It exists so the
// download tests can exercise the resume and verification paths without a
// device; the offset the command asked to resume from is recorded.
type stubDataClient struct {
	downloadOffset int64
}

func unimplemented(method string) error {
	return status.Errorf(codes.Unimplemented, "stubDataClient does not serve %s", method)
}

func (s *stubDataClient) Sources(context.Context, *agentpbv2.DataSourcesRequest, ...grpc.CallOption) (*agentpbv2.DataSourcesResponse, error) {
	return nil, unimplemented("Sources")
}
func (s *stubDataClient) Start(context.Context, *agentpbv2.DataStartRequest, ...grpc.CallOption) (*agentpbv2.DataEpisode, error) {
	return nil, unimplemented("Start")
}
func (s *stubDataClient) Stop(context.Context, *agentpbv2.DataStopRequest, ...grpc.CallOption) (*agentpbv2.DataEpisode, error) {
	return nil, unimplemented("Stop")
}
func (s *stubDataClient) Status(context.Context, *agentpbv2.DataStatusRequest, ...grpc.CallOption) (*agentpbv2.DataStatusResponse, error) {
	return nil, unimplemented("Status")
}
func (s *stubDataClient) Episodes(context.Context, *agentpbv2.DataEpisodesRequest, ...grpc.CallOption) (*agentpbv2.DataEpisodesResponse, error) {
	return nil, unimplemented("Episodes")
}
func (s *stubDataClient) Inspect(context.Context, *agentpbv2.DataInspectRequest, ...grpc.CallOption) (*agentpbv2.DataInspectResponse, error) {
	return nil, unimplemented("Inspect")
}
func (s *stubDataClient) Download(_ context.Context, in *agentpbv2.DataDownloadRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpbv2.DataDownloadChunk], error) {
	s.downloadOffset = in.GetOffset()
	return &exhaustedDownloadStream{}, nil
}
func (s *stubDataClient) CampaignDeploy(context.Context, *agentpbv2.DataCampaignDeployRequest, ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	return nil, unimplemented("CampaignDeploy")
}
func (s *stubDataClient) Campaigns(context.Context, *agentpbv2.DataCampaignsRequest, ...grpc.CallOption) (*agentpbv2.DataCampaignsResponse, error) {
	return nil, unimplemented("Campaigns")
}
func (s *stubDataClient) CampaignInspect(context.Context, *agentpbv2.DataCampaignInspectRequest, ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	return nil, unimplemented("CampaignInspect")
}
func (s *stubDataClient) CampaignTrigger(context.Context, *agentpbv2.DataCampaignTriggerRequest, ...grpc.CallOption) (*agentpbv2.DataEpisode, error) {
	return nil, unimplemented("CampaignTrigger")
}

// exhaustedDownloadStream yields no chunks. grpc.ClientStream is embedded for
// the methods downloadOne never calls; calling one panics rather than silently
// reporting a zero value.
type exhaustedDownloadStream struct {
	grpc.ClientStream
}

func (e *exhaustedDownloadStream) Recv() (*agentpbv2.DataDownloadChunk, error) { return nil, io.EOF }

// TestEncodeProtoJSONDialect pins the documented --json dialect: protocol
// buffer field names as the .proto declares them, one compact line, and stable
// bytes across runs (protojson randomises its whitespace, so the encoder
// compacts before writing).
func TestEncodeProtoJSONDialect(t *testing.T) {
	message := &agentpbv2.DataEpisode{Id: "ep1", Name: "commissioning", State: "sealed", StartedUnixNanos: 5, SizeBytes: 9}

	var first, second bytes.Buffer
	if err := encodeProtoJSON(&first, message); err != nil {
		t.Fatalf("encodeProtoJSON: %v", err)
	}
	if err := encodeProtoJSON(&second, message); err != nil {
		t.Fatalf("encodeProtoJSON: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("output is not stable across runs:\n%s\n%s", first.String(), second.String())
	}

	got := first.String()
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("output is not newline terminated: %q", got)
	}
	line := strings.TrimSuffix(got, "\n")
	if strings.ContainsAny(line, "\n\t") {
		t.Fatalf("output is not a single compact line: %q", got)
	}
	for _, want := range []string{`"id":"ep1"`, `"started_unix_nanos"`, `"size_bytes"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("output %q does not carry %s; the dialect is protocol buffer field names", line, want)
		}
	}
	if strings.Contains(line, "startedUnixNanos") {
		t.Fatalf("output %q uses lowerCamelCase; the dialect is protocol buffer field names", line)
	}
	if !json.Valid([]byte(line)) {
		t.Fatalf("output is not valid JSON: %q", line)
	}
	// Unpopulated fields stay out, exactly as protobuf JSON specifies.
	if strings.Contains(line, "boot_id") {
		t.Fatalf("output %q carries an unpopulated field", line)
	}

	var canonical bytes.Buffer
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Compact(&canonical, raw); err != nil {
		t.Fatal(err)
	}
	if line != canonical.String() {
		t.Fatalf("output %q is not canonical protobuf JSON %q", line, canonical.String())
	}
}
