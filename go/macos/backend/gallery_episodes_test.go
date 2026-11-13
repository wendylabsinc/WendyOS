package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"io"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type galleryCloseCounter struct{ count int }

func (c *galleryCloseCounter) Close() error { c.count++; return nil }

func TestGalleryIndexPreservesActiveSessionAndReusesConnection(t *testing.T) {
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
		defer b.galleryReset(true)
		if dialFailure && err == nil {
			t.Fatal("missing error")
		}
		if !dialFailure && (err != nil || len(rows) != 2) {
			t.Fatalf("%v %v", rows, err)
		}
		expectedClosed := 0
		if dialFailure {
			expectedClosed = 1
		}
		if closed.count != expectedClosed {
			t.Fatalf("temporary connection closed %d times", closed.count)
		}
		if b.conn != active || b.session != 42 || b.deviceSelector != "active" {
			t.Fatal("active connection changed")
		}
	}
}

func TestRecordedThermalMaximumUsesOnlyNumericCelsiusEvidence(t *testing.T) {
	payload := []byte(`{"attributes":{"max_celsius":23.6875}}
{"attributes":{"max_celsius":22.84375}}
{"attributes":{"max_celsius":"999"}}
{"attributes":{"max_celsius":-300}}
{"attributes":{"temperature":9000}}
invalid
`)
	got := recordedThermalMaximum(payload)
	if got == nil || *got != 23.6875 {
		t.Fatalf("wrong max %v", got)
	}
	if recordedThermalMaximum([]byte(`{"attributes":{}}`)) != nil {
		t.Fatal("invented missing measurement")
	}
	zero := recordedThermalMaximum([]byte(`{"attributes":{"max_celsius":0}}`))
	if zero == nil || *zero != 0 {
		t.Fatal("lost a real zero reading")
	}
}

func TestVisualPreviewRequiresMatchingManifestAndVerifiedThermalEvidence(t *testing.T) {
	payload := []byte(`{"attributes":{"max_celsius":23.6875}}`)
	sum := sha256.Sum256(payload)
	file := data.File{Path: "events.jsonl", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	client := &visualFixtureClient{manifest: data.Manifest{ID: "episode", Files: []data.File{file}}, files: map[string][]byte{"events.jsonl": payload}}
	preview, err := readVisualPreview(context.Background(), client, "episode")
	if err != nil || preview.MaximumCelsius == nil || *preview.MaximumCelsius != 23.6875 {
		t.Fatalf("%+v %v", preview, err)
	}
	if _, err := readVisualPreview(context.Background(), client, "other"); err == nil {
		t.Fatal("accepted wrong episode")
	}
	client.files["events.jsonl"] = []byte(`{"attributes":{"max_celsius":999}}`)
	preview, err = readVisualPreview(context.Background(), client, "episode")
	if err != nil || preview.MaximumCelsius != nil {
		t.Fatal("unverified thermal value displayed")
	}
}
