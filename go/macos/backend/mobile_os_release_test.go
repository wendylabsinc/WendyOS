package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type osReleaseVersionClient struct {
	agentpb.WendyAgentServiceClient
}

func (osReleaseVersionClient) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{OsVersion: proto.String("WendyOS-0.18.2"), DeviceType: proto.String("jetson-agx-thor"), StorageMedium: proto.String("nvme"), Featureset: []string{"wendyos-update"}}, ctx.Err()
}

type osReleaseCatalog func(*http.Request) (*http.Response, error)

func (f osReleaseCatalog) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A slow release catalog must not stall device switching or other operations,
// and a lookup in flight must still end with its connection.
func TestCheckOSReleaseFetchesCatalogWithoutBackendLock(t *testing.T) {
	files := map[string]string{
		"/wendyos-images-public/manifests/master.json": `{"devices":{"jetson-agx-thor":{"latest":"0.21.2","manifest_path":"manifests/thor.json"}}}`,
		"/wendyos-images-public/manifests/thor.json":   `{"versions":{"0.21.2":{"ota_update_path":"thor/default.wendy","nvme_ota_update_path":"thor/nvme.wendy"}}}`,
	}
	fetching, release := make(chan struct{}, 4), make(chan struct{})
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = osReleaseCatalog(func(req *http.Request) (*http.Response, error) {
		fetching <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		body, ok := files[req.URL.Path]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	b := New()
	defer b.tunnelSession.SetActive(false)
	defer b.Disconnect()
	type outcome struct {
		value *commands.DesktopOSRelease
		err   error
	}
	check := func() <-chan outcome {
		t.Helper()
		b.mu.Lock()
		b.conn = &grpcclient.AgentConnection{AgentService: osReleaseVersionClient{}}
		b.life, b.stop = context.WithCancel(context.Background())
		b.session++
		session := b.session
		b.mu.Unlock()
		out := make(chan outcome, 1)
		go func() {
			value, err := b.CheckOSRelease(session)
			out <- outcome{value, err}
		}()
		select {
		case <-fetching:
		case <-time.After(2 * time.Second):
			t.Fatal("release catalog fetch did not start")
		}
		return out
	}
	returns := func(op func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- op() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("backend operation waited for the public release catalog")
		}
	}
	result := func(pending <-chan outcome) outcome {
		t.Helper()
		select {
		case got := <-pending:
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("release lookup did not finish")
		}
		return outcome{}
	}

	pending := check()
	returns(b.Disconnect)
	if got := result(pending); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("disconnect did not cancel the release lookup: %+v", got)
	}

	pending = check()
	returns(b.StopVPN)
	close(release)
	got := result(pending)
	want := commands.DesktopOSRelease{DeviceType: "jetson-agx-thor", Installed: "WendyOS-0.18.2", Latest: "0.21.2", ArtifactURL: "https://storage.googleapis.com/wendyos-images-public/thor/nvme.wendy", Available: true}
	if got.err != nil || got.value == nil || *got.value != want {
		t.Fatalf("wrong release: %+v %v", got.value, got.err)
	}
}
