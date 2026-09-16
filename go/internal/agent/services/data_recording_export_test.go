package services

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingExportServer struct {
	grpc.ServerStream
	ctx     context.Context
	records []*recordingpb.StoredRecord
}

func (s *recordingExportServer) Context() context.Context { return s.ctx }
func (s *recordingExportServer) Send(r *recordingpb.StoredRecord) error {
	s.records = append(s.records, r)
	return nil
}
func TestRecordingExportIdentityAndValidation(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := appconfig.RecordingStream{Mode: "durable", MediaType: "text/csv"}
	if _, err = m.RecordStream("test.app", "worker", "samples", cfg, &recordingpb.Record{Id: "one", Payload: []byte("1,2\n")}); err != nil {
		t.Fatal(err)
	}
	service := &DataService{manager: m}
	out := &recordingExportServer{ctx: context.Background()}
	req := &agentpbv2.DataRecordingExportRequest{AppId: "test.app", Service: "worker", Stream: "samples"}
	if err = service.ExportRecording(req, out); err != nil {
		t.Fatal(err)
	}
	if len(out.records) != 1 || out.records[0].Service != "worker" || out.records[0].StreamDescriptor.MediaType != "text/csv" {
		t.Fatal("export lost identity or media type")
	}
	req.Service = "other"
	if err = service.ExportRecording(req, out); status.Code(err) != codes.NotFound {
		t.Fatalf("wrong service: %v", err)
	}
	req.Stream = "../escape"
	if err = service.ExportRecording(req, out); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsafe stream: %v", err)
	}
}
