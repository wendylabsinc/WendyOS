package peoplecamera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cameraAPI struct {
	appv1.WorldAppServiceClient
	media    []byte
	requests []serviceorigin.PublishRequest
}

func (a *cameraAPI) PutEvidence(_ context.Context, p *appv1.PutEvidenceRequest, _ ...grpc.CallOption) (*appv1.PutEvidenceReply, error) {
	a.media = append(a.media, p.Data...)
	return &appv1.PutEvidenceReply{Sha256: p.Sha256, Size: p.Size, ContentType: p.ContentType, Received: p.Offset + int64(len(p.Data)), Complete: p.Offset+int64(len(p.Data)) == p.Size, RetainUntilUnixMs: 1_800_000_000_000}, nil
}

func (a *cameraAPI) Publish(_ context.Context, p *appv1.PublishRequest, _ ...grpc.CallOption) (*appv1.PublishReply, error) {
	request, err := appv1.DecodePublish(p)
	if err != nil {
		return nil, err
	}
	a.requests = append(a.requests, request)
	return nil, status.Error(codes.Unavailable, "test lost acknowledgment")
}

func TestHTTPPathAndPublicationPreserveCameraIdentity(t *testing.T) {
	for _, sensor := range []string{"camera-a", "camera-b", "people-front", "people-oblique"} {
		t.Run(sensor, func(t *testing.T) {
			capture, cfg := namedFixture(t, sensor)
			raw, _ := json.Marshal(capture)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/cameras/"+sensor || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
					t.Error("source request identity changed")
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			cfg.SceneURL, cfg.ReadTokenFile = server.URL, filepath.Join(t.TempDir(), "token")
			cfg.DurationMS, cfg.MaxSamples, cfg.PollIntervalMS, cfg.TTLMS, cfg.RequestTimeoutMS = 3000, 1, 50, 3000, 1500
			if err := os.WriteFile(cfg.ReadTokenFile, []byte(strings.Repeat("t", 32)), 0600); err != nil {
				t.Fatal(err)
			}
			api, emitted := &cameraAPI{}, 0
			err := Run(context.Background(), cfg, api, func(liveapp.Receipt) error { emitted++; return nil })
			if !errors.Is(err, liveapp.ErrUncertain) || len(api.requests) != 1 || emitted != 0 || !bytes.Equal(api.media, capture.Media) {
				t.Fatal("publication/media changed or lost acknowledgment was retried", err, len(api.requests), emitted)
			}
			got, err := sensorrecord.ValidatePublication(api.requests[0])
			if err != nil || api.requests[0].Stream != sensor || !bytes.Equal(got.SampleJSON, capture.Capture.SampleJSON) || got.CapturedUnixNS != capture.Capture.CapturedUnixNS {
				t.Fatal("publication changed original camera capture", err)
			}
		})
	}
}
