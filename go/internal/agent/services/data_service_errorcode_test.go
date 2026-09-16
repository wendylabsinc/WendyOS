package services

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDataStatusErrorClassification pins the gRPC code returned for each class
// of data-manager error, guarding against a regression to the old behavior
// that collapsed every failure to NotFound.
func TestDataStatusErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"invalid episode id", data.ErrInvalidEpisodeID, codes.InvalidArgument},
		{"invalid offset", data.ErrInvalidDownloadOffset, codes.InvalidArgument},
		{"invalid path", data.ErrInvalidEpisodePath, codes.InvalidArgument},
		{"path escapes", data.ErrEpisodePathEscapes, codes.InvalidArgument},
		{"invalid campaign name", data.ErrInvalidCampaignName, codes.InvalidArgument},
		{"absent episode", os.ErrNotExist, codes.NotFound},
		{"no active episode", data.ErrNoActiveEpisode, codes.FailedPrecondition},
		{"entry not regular", data.ErrEpisodeEntryNotRegular, codes.FailedPrecondition},
		{"io failure", errors.New("disk exploded"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := status.Code(dataStatusError(tc.err))
			if got != tc.want {
				t.Fatalf("dataStatusError(%v) code = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// sealedEpisode records a minimal applications-only episode and returns its ID
// plus the manager, for the RPC-level error-code tests below.
func sealedEpisode(t *testing.T) (*DataService, string) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	started, err := manager.Start(data.StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	// Record one event so events.jsonl is sealed into the manifest; the
	// out-of-range-offset case below needs a file that actually exists.
	if _, err = manager.RecordApplication("com.example.test", data.ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootID: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Stop(data.AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	return service, started.ID
}

func TestInspectErrorCodes(t *testing.T) {
	service, id := sealedEpisode(t)

	if _, err := service.Inspect(context.Background(), &agentpbv2.DataInspectRequest{Episode: "bad/id"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid episode id: code = %s, want InvalidArgument (%v)", status.Code(err), err)
	}
	if _, err := service.Inspect(context.Background(), &agentpbv2.DataInspectRequest{Episode: "does-not-exist"}); status.Code(err) != codes.NotFound {
		t.Fatalf("absent episode: code = %s, want NotFound (%v)", status.Code(err), err)
	}
	if _, err := service.Inspect(context.Background(), &agentpbv2.DataInspectRequest{Episode: id, Verify: true}); err != nil {
		t.Fatalf("present episode should inspect cleanly: %v", err)
	}
}

// fakeDownloadStream is a minimal DataService_DownloadServer for driving the
// Download handler in-process. Only Send and Context are exercised.
type fakeDownloadStream struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*agentpbv2.DataDownloadChunk
}

func (f *fakeDownloadStream) Send(c *agentpbv2.DataDownloadChunk) error {
	f.chunks = append(f.chunks, c)
	return nil
}
func (f *fakeDownloadStream) Context() context.Context { return f.ctx }

func TestDownloadErrorCodes(t *testing.T) {
	service, id := sealedEpisode(t)
	stream := &fakeDownloadStream{ctx: context.Background()}

	if err := service.Download(&agentpbv2.DataDownloadRequest{Episode: "does-not-exist", Path: "events.jsonl"}, stream); status.Code(err) != codes.NotFound {
		t.Fatalf("absent episode: code = %s, want NotFound (%v)", status.Code(err), err)
	}
	// A path that is not a manifest entry (including a traversal attempt) is
	// genuinely absent, so NotFound is the honest code; path validation only
	// runs once a manifest match is found.
	if err := service.Download(&agentpbv2.DataDownloadRequest{Episode: id, Path: "../escape"}, stream); status.Code(err) != codes.NotFound {
		t.Fatalf("escaping path: code = %s, want NotFound (%v)", status.Code(err), err)
	}
	if err := service.Download(&agentpbv2.DataDownloadRequest{Episode: id, Path: "no-such-file.bin"}, stream); status.Code(err) != codes.NotFound {
		t.Fatalf("missing file: code = %s, want NotFound (%v)", status.Code(err), err)
	}
	if err := service.Download(&agentpbv2.DataDownloadRequest{Episode: id, Path: "events.jsonl", Offset: 1 << 40}, stream); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("out-of-range offset: code = %s, want InvalidArgument (%v)", status.Code(err), err)
	}
}
