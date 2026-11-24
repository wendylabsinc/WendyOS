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

// agentGeneration is what the agent that listed a device's camera rows does
// with a camera entitlement's format, stamp and allowlist, as far as its rows
// tell.
type agentGeneration int

const (
	// agentUndetermined shares no camera present and says no more: it is one
	// of the two generations below.
	agentUndetermined agentGeneration = iota
	// agentWithoutNodeSettings reports only share_mode. Every camera it shares
	// gets pictures stamped with capture times: it reads no format, stamp or
	// allowlist.
	agentWithoutNodeSettings
	// agentMatchingPathOrName reports a mode or a reason per format, and
	// matches an allowlist entry only against a camera's exact device path or
	// exact name.
	agentMatchingPathOrName
	// agentMatchingStableIDs puts the stable_id key on the rows of the cameras
	// it lists, and also matches stable ids and "v4l2:" paths.
	agentMatchingStableIDs
)

func agentGenerationOf(cams []*cameraRow) agentGeneration {
	generation := agentUndetermined
	for _, c := range cams {
		props := c.GetProperties()
		if _, keyed := props[camerashare.KeyStableID]; keyed {
			return agentMatchingStableIDs
		}
		for _, format := range appconfig.CameraFormats[1:] {
			if props[camerashare.KeyModeOfFormat(format)] != "" || props[camerashare.KeyUnavailableOfFormat(format)] != "" {
				generation = agentMatchingPathOrName
			}
		}
		if generation == agentUndetermined && props[camerashare.KeyMode] != "" {
			generation = agentWithoutNodeSettings
		}
	}
	return generation
}

// nodeSettingsOf resolves a camera's node settings as its agent will.
func nodeSettingsOf(entitlements []appconfig.Entitlement, c *cameraRow, generation agentGeneration) (format, stamp string, conflicts []string) {
	switch generation {
	case agentMatchingStableIDs:
		return appconfig.CameraNodeSettings(entitlements, cameraIdentityOf(c))
	case agentWithoutNodeSettings:
		return appconfig.CameraFormats[0], appconfig.CameraStamps[0], nil
	}
	return appconfig.CameraNodeSettingsByPathOrName(entitlements, c.GetDevicePath(), cameraNameOf(c.GetDescription()))
}

// entryNames reports whether the camera's agent matches an allowlist entry to
// the camera, or would if it read allowlists.
func entryNames(entry string, c *cameraRow, generation agentGeneration) bool {
	if generation == agentMatchingStableIDs {
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
	return agentGenerationOf(cams) != agentUndetermined || slices.ContainsFunc(cams, func(c *cameraRow) bool {
		return c.GetProperties()[camerashare.KeyUnavailable] != ""
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
	generation := agentGenerationOf(cams)
	var entries []string
	for _, c := range cams {
		for _, cfg := range sharingConfigs(cfgs) {
			asked, stamp, _ := nodeSettingsOf(cfg.Entitlements, c, generation)
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
// entries differ on a setting as the device's agent matches them, in the
// resolver's sentences. An agent built with this resolver logs the same ones
// and keeps them in the camera's share state.
func cameraSettingsConflictNotices(cfgs []*appconfig.AppConfig, cams []*cameraRow) []string {
	generation := agentGenerationOf(cams)
	var lines []string
	for _, c := range cams {
		if !sharedThroughANode(c) {
			continue
		}
		for _, cfg := range sharingConfigs(cfgs) {
			if _, _, conflicts := nodeSettingsOf(cfg.Entitlements, c, generation); len(conflicts) > 0 {
				lines = appendOnce(lines, fmt.Sprintf("Camera: %s: %s.", c.GetDescription(), strings.Join(conflicts, "; ")))
			}
		}
	}
	return lines
}

// isLaterSpelling reports whether an allowlist entry is written in a form
// only an agent that matches stable ids reads: a path after "v4l2:", or a
// stable id with its scheme.
func isLaterSpelling(entry string) bool {
	return slices.ContainsFunc([]string{"v4l2:", "by-id:", "by-path:"}, func(scheme string) bool { return strings.HasPrefix(entry, scheme) })
}

// cameraAllowlistNotices returns a line for each allowlist entry of a sharing
// config that configures no node: one that selects no shared camera, or names
// a camera the app reads without a share node. Against an agent that matches
// only a device path or a name, the entries written in a later spelling share
// one line that says so. It returns nothing for an agent that reads no
// allowlist: agentWithoutNodeSettingsNotice speaks for that one.
func cameraAllowlistNotices(cfgs []*appconfig.AppConfig, cams []*cameraRow) []string {
	generation := agentGenerationOf(cams)
	if generation == agentWithoutNodeSettings {
		return nil
	}
	var lines, laterSpellings []string
	for _, cfg := range sharingConfigs(cfgs) {
		for _, entry := range appconfig.CameraAllowlistEntries(cfg.Entitlements) {
			var named []*cameraRow
			for _, c := range cams {
				if entryNames(entry, c, generation) {
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
			case generation == agentMatchingPathOrName && isLaterSpelling(entry):
				laterSpellings = appendOnce(laterSpellings, fmt.Sprintf("%q", entry))
			default:
				lines = appendOnce(lines, fmt.Sprintf("Camera: allowlist entry %q selects no shared camera's node on this device, so its entitlement's format and stamp reach none. "+
					"\"wendy device camera list --json\" prints each camera's stable_id, path and name.", entry))
			}
		}
	}
	if len(laterSpellings) > 0 {
		lines = append(lines, fmt.Sprintf("Camera: the agent on this device predates stable id and \"v4l2:\" allowlist entries and matches an entry only against a camera's device path or name, "+
			"so these select no camera's node: %s. Update the agent, or name the camera by its device path or name.", strings.Join(laterSpellings, ", ")))
	}
	return lines
}

// agentWithoutNodeSettingsNotice returns the line for an app that sets a
// camera entry's format, stamp or allowlist on a device whose agent reads none
// of them, and "" otherwise.
func agentWithoutNodeSettingsNotice(cfgs []*appconfig.AppConfig, cams []*cameraRow) string {
	if agentGenerationOf(cams) != agentWithoutNodeSettings {
		return ""
	}
	for _, cfg := range sharingConfigs(cfgs) {
		for _, e := range cfg.Entitlements {
			isCamera := e.Type == appconfig.EntitlementCamera || e.Type == appconfig.EntitlementVideo
			if isCamera && (e.Format != "" || e.Stamp != "" || len(e.Allowlist) > 0) {
				return "Camera: the agent on this device does not read a camera entitlement's format, stamp or allowlist: " +
					"every shared camera's node carries pictures stamped with the capture time until the agent is updated."
			}
		}
	}
	return ""
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
	if line := agentWithoutNodeSettingsNotice(cfgs, resp.GetCapabilities()); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, cameraSettingsConflictNotices(cfgs, resp.GetCapabilities())...)
	lines = append(lines, cameraAllowlistNotices(cfgs, resp.GetCapabilities())...)
	for _, line := range lines {
		cliNotice("%s", line)
	}
}
