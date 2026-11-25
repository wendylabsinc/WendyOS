package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type driverUploadAgent struct {
	osDownloadClient
	version *agentpb.GetAgentVersionResponse
}

func (a *driverUploadAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return a.version, nil
}

func TestDriverUploadOverIsolatedConnection(t *testing.T) {
	payload := bytes.Repeat([]byte("driver"), driverChunkSize/3)
	digest := sha256.Sum256(payload)
	for _, stage := range []bool{false, true} {
		t.Run(fmt.Sprintf("stage=%v", stage), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			var installs []*fakeInstallStream
			conn := &grpcclient.AgentConnection{
				Host: "169.254.243.154",
				AgentService: &driverUploadAgent{version: &agentpb.GetAgentVersionResponse{
					NetworkInterfaces: []*agentpb.NetworkInterface{{IpAddresses: []string{"169.254.243.154"}}},
				}},
				DriverService: fakeDriverClient{installs: &installs},
			}
			spec := &agentpbv2.DriverSpec{
				Name: "intel-be202", Version: "2026.10.26", KernelVersion: "kernel",
				Sha256: hex.EncodeToString(digest[:]), Signature: []byte{1, 2, 3},
				ModulesLoad: []string{"iwlmld"}, ArtifactUrl: server.URL, StageOnly: stage,
			}
			original := proto.Clone(spec).(*agentpbv2.DriverSpec)
			var err error
			if stage {
				err = runDriverStage(context.Background(), conn, spec)
			} else {
				err = installDriver(context.Background(), conn, spec, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || len(installs) != 1 || len(installs[0].specs) != 1 {
				t.Fatalf("downloads=%d installs=%v", requests, installs)
			}
			want := proto.Clone(original).(*agentpbv2.DriverSpec)
			want.ArtifactUrl = ""
			if !proto.Equal(installs[0].specs[0], want) || !proto.Equal(spec, original) {
				t.Fatalf("published metadata changed: uploaded=%v original=%v", installs[0].specs[0], spec)
			}
			if !bytes.Equal(installs[0].data, payload) {
				t.Fatal("uploaded bytes differ from the published image")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary downloads remain: %v, %v", entries, err)
			}
		})
	}
}

func TestDriverUploadKeepsDirectDownloadForNetworkedDevice(t *testing.T) {
	var installs []*fakeInstallStream
	conn := &grpcclient.AgentConnection{
		Host: "169.254.243.154",
		AgentService: &driverUploadAgent{version: &agentpb.GetAgentVersionResponse{
			NetworkInterfaces: []*agentpb.NetworkInterface{{IpAddresses: []string{"192.168.1.2"}}},
		}},
		DriverService: fakeDriverClient{installs: &installs},
	}
	spec := &agentpbv2.DriverSpec{Name: "driver", ArtifactUrl: "https://example.invalid/driver.raw"}
	if err := installDriver(context.Background(), conn, spec, ""); err != nil {
		t.Fatal(err)
	}
	if len(installs) != 1 || !proto.Equal(installs[0].specs[0], spec) || len(installs[0].data) != 0 {
		t.Fatalf("networked device should fetch the original URL: %v", installs)
	}
}

func TestDriverUploadDownloadFailuresDoNotInstall(t *testing.T) {
	for _, failure := range []string{"missing digest", "checksum", "HTTP", "truncated", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if failure == "HTTP" {
					w.WriteHeader(http.StatusNotFound)
				}
				if failure == "truncated" {
					w.Header().Set("Content-Length", "100")
				}
				_, _ = w.Write([]byte("image"))
			}))
			defer server.Close()
			var installs []*fakeInstallStream
			conn := &grpcclient.AgentConnection{
				Host: "169.254.243.154", AgentService: &driverUploadAgent{},
				DriverService: fakeDriverClient{installs: &installs},
			}
			digest := sha256.Sum256([]byte("image"))
			spec := &agentpbv2.DriverSpec{Name: "driver", ArtifactUrl: server.URL, Sha256: hex.EncodeToString(digest[:])}
			if failure == "missing digest" {
				spec.Sha256 = ""
			}
			if failure == "checksum" {
				spec.Sha256 = hex.EncodeToString(make([]byte, sha256.Size))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled" {
				cancel()
			}
			err := installDriver(ctx, conn, spec, "")
			if err == nil || len(installs) != 0 {
				t.Fatalf("failed download reached the device: err=%v installs=%v", err, installs)
			}
			if failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial download remains: %v, %v", entries, err)
			}
		})
	}
}
