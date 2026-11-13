package commands

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type osUpdateTestStream struct {
	grpc.ServerStreamingClient[agentpb.UpdateOSResponse]
	frames []*agentpb.UpdateOSResponse
	err    error
}

func (s *osUpdateTestStream) Recv() (*agentpb.UpdateOSResponse, error) {
	if len(s.frames) == 0 {
		return nil, s.err
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

func TestOSUpdateStreamRequiresCompletion(t *testing.T) {
	progress := &agentpb.UpdateOSResponse{ResponseType: &agentpb.UpdateOSResponse_Progress_{
		Progress: &agentpb.UpdateOSResponse_Progress{Phase: "finalizing", Percent: 100},
	}}
	completed := &agentpb.UpdateOSResponse{ResponseType: &agentpb.UpdateOSResponse_Completed_{
		Completed: &agentpb.UpdateOSResponse_Completed{RebootRequired: true},
	}}
	failed := &agentpb.UpdateOSResponse{ResponseType: &agentpb.UpdateOSResponse_Failed_{
		Failed: &agentpb.UpdateOSResponse_Failed{ErrorMessage: "disk full"},
	}}
	transportErr := status.Error(codes.Unavailable, "disconnected")
	for _, tc := range []struct {
		name   string
		frames []*agentpb.UpdateOSResponse
		err    error
		want   error
		text   string
	}{
		{name: "empty EOF", err: io.EOF, want: io.ErrUnexpectedEOF},
		{name: "final progress is not completion", frames: []*agentpb.UpdateOSResponse{progress}, err: io.EOF, want: io.ErrUnexpectedEOF},
		{name: "explicit completion", frames: []*agentpb.UpdateOSResponse{progress, completed}, err: io.EOF},
		{name: "explicit failure", frames: []*agentpb.UpdateOSResponse{failed}, err: io.EOF, text: "disk full"},
		{name: "transport failure", frames: []*agentpb.UpdateOSResponse{progress}, err: transportErr, want: transportErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := drainOSUpdateStream(&osUpdateTestStream{frames: tc.frames, err: tc.err})
			if tc.text != "" {
				if err == nil || !strings.Contains(err.Error(), tc.text) {
					t.Fatalf("error = %v, want %q", err, tc.text)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
