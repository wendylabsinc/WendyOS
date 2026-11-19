package commands

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type hwCap = agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

func shareCfg(mode string) *appconfig.AppConfig {
	return &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: mode}}}
}

func shareRow(desc, mode string) *hwCap {
	props := map[string]string{}
	if mode != "" {
		props[camerashare.KeyMode] = mode
	}
	return &hwCap{Category: "camera", Description: desc, Properties: props}
}

func TestCameraShareNotice_OneLineNamingEachCameraAndDirect(t *testing.T) {
	got := cameraShareNotice([]*appconfig.AppConfig{shareCfg("")}, []*hwCap{
		shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps"),
		shareRow("C270 HD WEBCAM (video2)", "640x480 YUYV, 30 fps"),
	})
	want := `Camera: this app shares Brio 101 (video0) at 1280x720 YUYV, 30 fps; C270 HD WEBCAM (video2) at 640x480 YUYV, 30 fps. For the real device (its own formats, sizes and controls), set "mode": "direct" on the camera entitlement.`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCameraShareNotice_SilentWhenNothingWillBeShared(t *testing.T) {
	rows := []*hwCap{shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps")}
	if got := cameraShareNotice([]*appconfig.AppConfig{shareCfg("direct")}, rows); got != "" {
		t.Fatalf("direct app: %q", got)
	}
	noCam := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}}
	if got := cameraShareNotice([]*appconfig.AppConfig{noCam}, rows); got != "" {
		t.Fatalf("no camera entitlement: %q", got)
	}
	if got := cameraShareNotice([]*appconfig.AppConfig{shareCfg("")}, []*hwCap{shareRow("Brio 101 (video0)", "")}); got != "" {
		t.Fatalf("older agent: %q", got)
	}
}

func TestCameraShareNotice_ServicesShareOneLine(t *testing.T) {
	got := cameraShareNotice([]*appconfig.AppConfig{shareCfg(""), shareCfg("")}, []*hwCap{shareRow("Brio 101 (video0)", "1280x720 YUYV, 30 fps")})
	if strings.Contains(got, "\n") || strings.Count(got, "Camera:") != 1 {
		t.Fatalf("want one line, got %q", got)
	}
}

type blockingHW struct {
	agentpb.WendyAgentServiceClient
}

func (blockingHW) ListHardwareCapabilities(ctx context.Context, _ *agentpb.ListHardwareCapabilitiesRequest, _ ...grpc.CallOption) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPrintCameraShareNotice_SlowAgentPrintsNothing(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	// Restored even when the test fails or panics before the read below.
	t.Cleanup(func() { os.Stderr = orig })
	start := time.Now()
	printCameraShareNotice(context.Background(), &grpcclient.AgentConnection{AgentService: blockingHW{}}, shareCfg(""))
	elapsed := time.Since(start)
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if elapsed > 3200*time.Millisecond {
		t.Fatalf("took %v", elapsed)
	}
	if len(out) != 0 {
		t.Fatalf("printed %q", out)
	}
}
