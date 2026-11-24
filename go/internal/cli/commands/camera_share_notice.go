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

type cameraRow = agentpb.ListHardwareCapabilitiesResponse_HardwareCapability

func cameraIdentityOf(c *cameraRow) appconfig.CameraIdentity {
	return appconfig.CameraIdentity{
		Path:     c.GetDevicePath(),
		StableID: c.GetProperties()[camerashare.KeyStableID],
		Name:     cameraNameOf(c.GetDescription()),
	}
}

// agentMatchesStableIDs reports whether the agent that listed cams matches an
// allowlist entry by stable id and "v4l2:" path. Such an agent puts the
// stable_id key on every camera row it reports on; one from before matches
// only a camera's exact device path or exact name.
func agentMatchesStableIDs(cams []*cameraRow) bool {
	return slices.ContainsFunc(cams, func(c *cameraRow) bool {
		_, keyed := c.GetProperties()[camerashare.KeyStableID]
		return keyed
	})
}

// nodeSettingsOf resolves a camera's node settings as its agent will.
func nodeSettingsOf(entitlements []appconfig.Entitlement, c *cameraRow, byStableID bool) (format, stamp string, conflicts []string) {
	if byStableID {
		return appconfig.CameraNodeSettings(entitlements, cameraIdentityOf(c))
	}
	return appconfig.CameraNodeSettingsByPathOrName(entitlements, c.GetDevicePath(), cameraNameOf(c.GetDescription()))
}

// entryNames reports whether the camera's agent matches an allowlist entry to
// the camera.
func entryNames(entry string, c *cameraRow, byStableID bool) bool {
	if byStableID {
		return cameraIdentityOf(c).NamedBy(entry)
	}
	return entry != "" && (entry == c.GetDevicePath() || entry == cameraNameOf(c.GetDescription()))
}

// servedNodeOf returns the format and mode of the node the agent gives an app
// that asks for asked: the first format the camera can be declared in. Both
// are "" for a camera that is not shared through a node.
func servedNodeOf(c *cameraRow, asked string) (served, mode string) {
	for _, format := range appconfig.CameraFormatsServedFor(asked) {
		if mode := c.GetProperties()[sharedCameraModeKey(format)]; mode != "" {
			return format, mode
		}
	}
	return "", ""
}

func sharedThroughANode(c *cameraRow) bool {
	return slices.ContainsFunc(appconfig.CameraFormats, func(format string) bool {
		return c.GetProperties()[sharedCameraModeKey(format)] != ""
	})
}

// agentSharesCameras reports whether the agent's rows say camera sharing is in
// effect, with or without a camera that can be shared.
func agentSharesCameras(cams []*cameraRow) bool {
	return agentMatchesStableIDs(cams) || slices.ContainsFunc(cams, func(c *cameraRow) bool {
		return sharedThroughANode(c) || c.GetProperties()[camerashare.KeyUnavailable] != ""
	})
}

func sharingConfigs(cfgs []*appconfig.AppConfig) []*appconfig.AppConfig {
	var sharing []*appconfig.AppConfig
	for _, cfg := range cfgs {
		if cfg != nil && appconfig.CameraShareChoice(cfg.Entitlements) == appconfig.CameraShareShared {
			sharing = append(sharing, cfg)
		}
	}
	return sharing
}

func appendOnce(lines []string, line string) []string {
	if slices.Contains(lines, line) {
		return lines
	}
	return append(lines, line)
}

// cameraShareNotice returns the one line telling the developer that the app
// will share the camera(s) the agent reports a shared mode for, with the mode,
// format and stamp of the node each config's entitlements give it. A camera
// the agent cannot declare in the format asked for is served in another
// format, and its entry says so with the agent's reason. The line is "" when
// nothing will be shared: no config asks for sharing, or no camera row carries
// a share mode (an older agent, or no shareable camera).
func cameraShareNotice(cfgs []*appconfig.AppConfig, cams []*cameraRow) string {
	byStableID := agentMatchesStableIDs(cams)
	var entries []string
	for _, c := range cams {
		for _, cfg := range sharingConfigs(cfgs) {
			asked, stamp, _ := nodeSettingsOf(cfg.Entitlements, c, byStableID)
			served, mode := servedNodeOf(c, asked)
			if mode == "" {
				continue
			}
			instead := ""
			if served != asked {
				why := c.GetProperties()[camerashare.KeyUnavailableOfFormat(asked)]
				if why == "" {
					why = "the agent on this device does not offer that format"
				}
				instead = fmt.Sprintf(" (asked for %s; serving %s because %s)", asked, served, why)
			}
			entries = appendOnce(entries, fmt.Sprintf("%s at %s, format %s, stamp %s%s", c.GetDescription(), mode, served, stamp, instead))
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return fmt.Sprintf("Camera: this app shares %s. For the real device (its own formats, sizes and controls), set \"mode\": \"direct\" on the camera entitlement.", strings.Join(entries, "; "))
}

// cameraSettingsConflictNotices returns one line for each shared camera whose
// entries differ on a setting, in the sentences the agent logs and keeps in
// the camera's share state.
func cameraSettingsConflictNotices(cfgs []*appconfig.AppConfig, cams []*cameraRow) []string {
	byStableID := agentMatchesStableIDs(cams)
	var lines []string
	for _, c := range cams {
		if !sharedThroughANode(c) {
			continue
		}
		for _, cfg := range sharingConfigs(cfgs) {
			if _, _, conflicts := nodeSettingsOf(cfg.Entitlements, c, byStableID); len(conflicts) > 0 {
				lines = appendOnce(lines, fmt.Sprintf("Camera: %s: %s.", c.GetDescription(), strings.Join(conflicts, "; ")))
			}
		}
	}
	return lines
}

// cameraAllowlistNotices returns a line for each allowlist entry of a sharing
// config that configures no node: one that selects no shared camera, or names
// a camera the app reads without a share node. Against an agent from before
// stable ids, the entries that are neither a device path nor a camera's name
// share one line, since that agent matches nothing else.
func cameraAllowlistNotices(cfgs []*appconfig.AppConfig, cams []*cameraRow) []string {
	byStableID := agentMatchesStableIDs(cams)
	var lines, unmatchable []string
	for _, cfg := range sharingConfigs(cfgs) {
		for _, entry := range appconfig.CameraAllowlistEntries(cfg.Entitlements) {
			var named []*cameraRow
			for _, c := range cams {
				if entryNames(entry, c, byStableID) {
					named = append(named, c)
				}
			}
			switch {
			case slices.ContainsFunc(named, sharedThroughANode):
			case len(named) > 0:
				why := ""
				if reason := named[0].GetProperties()[camerashare.KeyUnavailable]; reason != "" {
					why = " (" + reason + ")"
				}
				lines = appendOnce(lines, fmt.Sprintf("Camera: allowlist entry %q names %s, which is not shared through a node%s, so the entry configures nothing.",
					entry, named[0].GetDescription(), why))
			case !byStableID && !strings.HasPrefix(entry, "/"):
				unmatchable = appendOnce(unmatchable, fmt.Sprintf("%q", entry))
			default:
				lines = appendOnce(lines, fmt.Sprintf("Camera: allowlist entry %q selects no shared camera's node on this device, so its entitlement's format and stamp reach none. "+
					"\"wendy device camera list --json\" prints each camera's stable_id, path and name.", entry))
			}
		}
	}
	if len(unmatchable) > 0 {
		lines = append(lines, fmt.Sprintf("Camera: the agent on this device predates stable id and \"v4l2:\" allowlist entries and matches an entry only against a camera's device path or name, "+
			"so these select no camera's node: %s. Update the agent, or name the camera by its device path or name.", strings.Join(unmatchable, ", ")))
	}
	return lines
}

// printCameraShareNotice prints the notice when the agent says the app's
// camera will be shared, then what its entries disagree on and which allowlist
// entries configure nothing. Any failure or delay prints nothing, and so does
// an agent whose rows say nothing of sharing.
func printCameraShareNotice(ctx context.Context, conn *grpcclient.AgentConnection, cfgs ...*appconfig.AppConfig) {
	if !anyCameraShared(cfgs) || conn == nil || conn.AgentService == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cameraShareNoticeTimeout)
	defer cancel()
	category := "camera"
	resp, err := conn.AgentService.ListHardwareCapabilities(ctx, &agentpb.ListHardwareCapabilitiesRequest{CategoryFilter: &category})
	if err != nil || !agentSharesCameras(resp.GetCapabilities()) {
		return
	}
	var lines []string
	if line := cameraShareNotice(cfgs, resp.GetCapabilities()); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, cameraSettingsConflictNotices(cfgs, resp.GetCapabilities())...)
	lines = append(lines, cameraAllowlistNotices(cfgs, resp.GetCapabilities())...)
	for _, line := range lines {
		cliNotice("%s", line)
	}
}
