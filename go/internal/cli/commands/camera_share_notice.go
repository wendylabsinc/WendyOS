package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraShareNoticeTimeout bounds the hardware call behind the notice; the
// line is a courtesy and must never hold up a deploy.
const cameraShareNoticeTimeout = 3 * time.Second

func anyCameraShared(cfgs []*appconfig.AppConfig) bool {
	for _, cfg := range cfgs {
		if cfg != nil && appconfig.CameraShareChoice(cfg.Entitlements) == appconfig.CameraShareShared {
			return true
		}
	}
	return false
}

// sharedCameraModeKey is the property under which the agent reports a
// camera's mode for a node of the given format.
func sharedCameraModeKey(format string) string {
	if format == appconfig.CameraFormats[0] {
		return camerashare.KeyMode
	}
	return camerashare.KeyModeOfFormat(format)
}

// cameraNameOf takes the camera's name out of a hardware row's description,
// "<name> (video<N>)".
func cameraNameOf(description string) string {
	if i := strings.LastIndex(description, " (video"); i >= 0 {
		return description[:i]
	}
	return description
}

// cameraShareNotice returns the one line telling the developer that the app
// will share the camera(s) the agent reports a shared mode for, with the mode,
// format and stamp of the node each config's entitlements give it, or "" when
// nothing will be shared: no config asks for sharing, or no camera row carries
// a share mode in the format asked for (an older agent, or no shareable
// camera).
func cameraShareNotice(cfgs []*appconfig.AppConfig, cams []*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability) string {
	if !anyCameraShared(cfgs) {
		return ""
	}
	var entries []string
	for _, c := range cams {
		for _, cfg := range cfgs {
			if cfg == nil || appconfig.CameraShareChoice(cfg.Entitlements) != appconfig.CameraShareShared {
				continue
			}
			format, stamp := appconfig.CameraNodeSettings(cfg.Entitlements, c.GetDevicePath(), cameraNameOf(c.GetDescription()))
			mode := c.GetProperties()[sharedCameraModeKey(format)]
			if mode == "" {
				continue
			}
			entry := fmt.Sprintf("%s at %s, format %s, stamp %s", c.GetDescription(), mode, format, stamp)
			if !slices.Contains(entries, entry) {
				entries = append(entries, entry)
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return fmt.Sprintf("Camera: this app shares %s. For the real device (its own formats, sizes and controls), set \"mode\": \"direct\" on the camera entitlement.", strings.Join(entries, "; "))
}

// printCameraShareNotice prints the notice when the agent says the app's
// camera will be shared. Any failure or delay prints nothing.
func printCameraShareNotice(ctx context.Context, conn *grpcclient.AgentConnection, cfgs ...*appconfig.AppConfig) {
	if !anyCameraShared(cfgs) || conn == nil || conn.AgentService == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cameraShareNoticeTimeout)
	defer cancel()
	category := "camera"
	resp, err := conn.AgentService.ListHardwareCapabilities(ctx, &agentpb.ListHardwareCapabilitiesRequest{CategoryFilter: &category})
	if err != nil {
		return
	}
	if line := cameraShareNotice(cfgs, resp.GetCapabilities()); line != "" {
		cliNotice("%s", line)
	}
}
