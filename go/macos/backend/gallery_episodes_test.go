package backend

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type galleryCloseCounter struct{ count int }

func (c *galleryCloseCounter) Close() error { c.count++; return nil }

func TestGalleryIndexPreservesActiveSessionAndClosesTemporaryConnection(t *testing.T) {
	for _, dialFailure := range []bool{false, true} {
		b := New()
		active := &grpcclient.AgentConnection{}
		b.conn, b.session, b.deviceSelector = active, 42, "active"
		closed := &galleryCloseCounter{}
		target := &grpcclient.AgentConnection{DataService: &campaignClipClient{episodes: []*pb.DataEpisode{{Id: "manual"}, {Id: "campaign"}}}, ExtraClosers: []io.Closer{closed}}
		b.connectTarget = func(ctx context.Context, selector string) (*grpcclient.AgentConnection, liteConnection, error) {
			if selector != "target" {
				t.Fatalf("wrong target %q", selector)
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 20*time.Second {
				t.Fatal("missing bounded deadline")
			}
			if dialFailure {
				return target, nil, errors.New("dial failed")
			}
			return target, nil, nil
		}
		rows, err := b.VisualEpisodesAt("target")
		if dialFailure && err == nil {
			t.Fatal("missing error")
		}
		if !dialFailure && (err != nil || len(rows) != 2) {
			t.Fatalf("%v %v", rows, err)
		}
		if closed.count != 1 {
			t.Fatalf("temporary connection closed %d times", closed.count)
		}
		if b.conn != active || b.session != 42 || b.deviceSelector != "active" {
			t.Fatal("active connection changed")
		}
	}
}
