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

// DesktopCheckOSRelease uses the CLI's stable OTA catalog and storage selection.
// It only reads device identity and release metadata; it never starts an update.
func DesktopCheckOSRelease(ctx context.Context, conn *grpcclient.AgentConnection) (*DesktopOSRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
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
