package commands

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type DesktopOSRelease struct {
	DeviceType    string `json:"deviceType"`
	Installed     string `json:"installed"`
	Latest        string `json:"latest"`
	ArtifactURL   string `json:"artifactURL"`
	Available     bool   `json:"available"`
	Current       bool   `json:"current"`
	BlockedReason string `json:"blockedReason"`
}

// DesktopOSReleaseIdentity reads the device identity an OS release check needs.
// It is the only step of the check that uses the device connection.
func DesktopOSReleaseIdentity(ctx context.Context, conn *grpcclient.AgentConnection) (*agentpb.GetAgentVersionResponse, error) {
	probeCtx, cancelProbe := context.WithTimeout(ctx, 8*time.Second)
	info, err := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	cancelProbe()
	if err != nil {
		return nil, err
	}
	if err := validateOSUpdateIdentity(info); err != nil {
		return nil, err
	}
	if info.GetDeviceType() == "" {
		return nil, errors.New("the device did not report its device type; automatic OS update checking is unavailable")
	}
	return info, nil
}

// DesktopResolveOSRelease uses the CLI's stable OTA catalog and storage selection.
// It reads only public release metadata, so callers need not hold the device
// connection while it waits on the network. It never starts an update.
func DesktopResolveOSRelease(ctx context.Context, info *agentpb.GetAgentVersionResponse) (*DesktopOSRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	artifact, latest, err := getLatestOTAInfoForDeviceTypeContext(ctx, info.GetDeviceType(), info.GetStorageMedium(), false)
	if err != nil {
		return nil, err
	}
	result := &DesktopOSRelease{
		DeviceType: info.GetDeviceType(), Installed: info.GetOsVersion(), Latest: latest, ArtifactURL: artifact,
		Current: osAlreadyCurrent(info.GetOsVersion(), latest, false),
	}
	result.Available = result.Installed != "" && !result.Current
	if result.Installed == "" {
		result.BlockedReason = "The device did not report its installed OS version."
	} else if err := requireReflashableOSVersion(result.Installed); err != nil {
		result.BlockedReason = err.Error()
	} else if err := osUpdateStackMismatch(info, artifact); err != nil {
		result.BlockedReason = err.Error()
	}
	return result, nil
}
