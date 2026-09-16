package services

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
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

func TestRecordingExportCheckpointRPC(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := appconfig.RecordingStream{Mode: "durable", MediaType: "application/octet-stream", Storage: &appconfig.RecordingStorage{RetentionSeconds: proto.Int64(0)}}
	record := func(id string) {
		t.Helper()
		if _, err := m.RecordStream("test.app", "worker", "vibration", cfg, &recordingpb.Record{Id: id, Payload: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	record("before")
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agentpbv2.RegisterDataServiceServer(server, NewDataService(m))
	go server.Serve(lis)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///recording", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agentpbv2.NewDataServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := client.ExportRecording(ctx, &agentpbv2.DataRecordingExportRequest{AppId: "test.app", Service: "worker", Stream: "vibration", Checkpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := out.Recv()
	if err != nil || r.Record.Id != "before" {
		t.Fatal("export", err)
	}
	if _, err = out.Recv(); err != io.EOF {
		t.Fatal("export end", err)
	}
	tokens := out.Trailer().Get("wendy-recording-checkpoint")
	if len(tokens) != 1 || tokens[0] == "" {
		t.Fatal("missing trailer")
	}
	record("after")
	ack := &agentpbv2.DataRecordingExportAckRequest{AppId: "test.app", Service: "worker", Stream: "vibration", Checkpoint: tokens[0]}
	if _, err = client.AcknowledgeRecordingExport(ctx, ack); err != nil {
		t.Fatal(err)
	}
	if _, err = client.AcknowledgeRecordingExport(ctx, ack); err != nil {
		t.Fatal("idempotent ack", err)
	}
	count := 0
	if err = m.ExportRecording("test.app", "worker", "vibration", func(r *recordingpb.StoredRecord) error {
		count++
		if r.Record.Id != "after" {
			t.Fatal("reclaimed wrong snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("missing concurrent data")
	}
	ack.Checkpoint = "forged"
	if _, err = client.AcknowledgeRecordingExport(ctx, ack); status.Code(err) != codes.InvalidArgument {
		t.Fatal("invalid token", err)
	}
}
