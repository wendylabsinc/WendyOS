package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type recordingExportClient struct {
	agentpbv2.DataServiceClient
	stream       *recordingExportClientStream
	acknowledged bool
	acknowledge  func(*agentpbv2.DataRecordingExportAckRequest) error
}

func (c *recordingExportClient) ExportRecording(context.Context, *agentpbv2.DataRecordingExportRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[recordingpb.StoredRecord], error) {
	return c.stream, nil
}
func (c *recordingExportClient) AcknowledgeRecordingExport(_ context.Context, r *agentpbv2.DataRecordingExportAckRequest, _ ...grpc.CallOption) (*agentpbv2.DataRecordingExportAckResponse, error) {
	c.acknowledged = true
	return &agentpbv2.DataRecordingExportAckResponse{}, c.acknowledge(r)
}

type recordingExportClientStream struct {
	grpc.ClientStream
	records []*recordingpb.StoredRecord
	token   string
	err     error
}

func (s *recordingExportClientStream) Recv() (*recordingpb.StoredRecord, error) {
	if len(s.records) > 0 {
		r := s.records[0]
		s.records = s.records[1:]
		return r, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}
func (s *recordingExportClientStream) Trailer() metadata.MD {
	return metadata.Pairs("wendy-recording-checkpoint", s.token)
}

func TestRecordingExportFileReclaimsOnlyCompletedOutput(t *testing.T) {
	for _, scenario := range []string{"success", "interrupted", "bad-record", "missing-token", "ack-lost", "snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "samples.wdr")
			r := &recordingpb.StoredRecord{Record: &recordingpb.Record{Id: "batch", Payload: []byte{1, 2, 3}}, StreamDescriptor: &recordingpb.StreamDescriptor{MediaType: "application/octet-stream"}}
			stream := &recordingExportClientStream{records: []*recordingpb.StoredRecord{r}, token: "opaque-token"}
			c := &recordingExportClient{stream: stream}
			c.acknowledge = func(req *agentpbv2.DataRecordingExportAckRequest) error {
				if req.AppId != "test.app" || req.Service != "worker" || req.Stream != "vibration" || req.Checkpoint != "opaque-token" {
					t.Fatal("ack identity mismatch", req)
				}
				f, err := os.Open(output)
				if err != nil {
					t.Fatal("ack before output exists", err)
				}
				defer f.Close()
				got, _, err := data.ReadRecording(f)
				if err != nil || got.Record.Id != "batch" {
					t.Fatal("ack before complete output", err)
				}
				if _, _, err = data.ReadRecording(f); err != io.EOF {
					t.Fatal("invalid export ending", err)
				}
				if scenario == "ack-lost" {
					return errors.New("connection lost after remote reclamation")
				}
				return nil
			}
			switch scenario {
			case "interrupted":
				stream.err = errors.New("disconnected before EOF")
			case "bad-record":
				r.StreamDescriptor = nil
			case "missing-token":
				stream.token = ""
			}
			reclaim := scenario != "snapshot"
			_, err := exportRecordingFile(context.Background(), c, &agentpbv2.DataRecordingExportRequest{AppId: "test.app", Service: "worker", Stream: "vibration", Checkpoint: reclaim}, output)
			success := scenario == "success" || scenario == "snapshot"
			if (err == nil) != success {
				t.Fatal("unexpected result", err)
			}
			wantAck := scenario == "success" || scenario == "ack-lost"
			if c.acknowledged != wantAck {
				t.Fatal("unsafe acknowledgement", c.acknowledged)
			}
			_, statErr := os.Stat(output)
			wantFile := scenario != "interrupted" && scenario != "bad-record"
			if (statErr == nil) != wantFile {
				t.Fatal("incorrect local retention", statErr)
			}
		})
	}
}
func TestRecordingExportNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.wdr")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The embedded nil client would panic if the command contacted the device.
	_, err := exportRecordingFile(context.Background(), &recordingExportClient{}, &agentpbv2.DataRecordingExportRequest{Checkpoint: true}, path)
	if !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "keep" {
		t.Fatal("overwrote destination")
	}
}
