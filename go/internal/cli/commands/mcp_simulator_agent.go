package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var simulatorUpdateConnect = connectSimulatorAgent
var simulatorUpdateResolve = resolveAgentArtifact
var simulatorUpdateUpload = deviceUpdateUpload

// Repair only the agent in an already running, locally owned robot VM. The
// download/upload/verification path is the same as the official CLI updater;
// no client-supplied endpoint, binary path, release URL, or physical selector
// enters this operation.
func updateMCPSimulatorAgent(ctx context.Context, name string) (*wendymcp.SimulatorAgentUpdate, error) {
	if err := vm.ValidName(name); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := robotVMStore()
	if err != nil {
		return nil, err
	}
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("simulator %q has no robot profile", name)
	}
	runtime, err := robotRuntimeForKind(profile.Kind)
	if err != nil {
		return nil, err
	}
	unlock, err := lockRobotProvision(ctx, store, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	statuses, err := vmStatusesFn()
	if err != nil {
		return nil, err
	}
	address := ""
	for _, status := range statuses {
		if status.Name == name && status.Running {
			address = vmAddress(status)
			break
		}
	}
	if address == "" {
		return nil, fmt.Errorf("simulator %q must be running before its agent can be updated", name)
	}
	conn, info, err := simulatorUpdateConnect(ctx, name, address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := verifySimulatorUpdateTarget(conn, info, name); err != nil {
		return nil, err
	}
	result := &wendymcp.SimulatorAgentUpdate{Name: name, Device: "vm:" + name, Version: info.GetVersion()}
	if slices.Contains(info.GetFeatureset(), runtime.agentFeature) {
		result.CapabilityVerified = true
		return result, nil
	}
	artifact, expectedVersion, _, err := simulatorUpdateResolve(info.GetOs(), info.GetCpuArchitecture(), false)
	if err != nil {
		return nil, fmt.Errorf("resolving official simulator agent: %w", err)
	}
	if err := checkLocalAgentArtifact(artifact, info.GetOs(), info.GetCpuArchitecture()); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(artifact)
	expectedHash := hex.EncodeToString(digest[:])
	if err := simulatorUpdateUpload(ctx, conn.AgentService, artifact, expectedHash); err != nil && !errors.Is(err, errAgentUpdateUnconfirmed) {
		return nil, fmt.Errorf("uploading simulator agent: %w", err)
	}
	_ = conn.Close()
	next, err := waitForUpdatedAgentReady(ctx, func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		fresh, version, err := simulatorUpdateConnect(ctx, name, address)
		if err != nil {
			return nil, err
		}
		if err := verifySimulatorUpdateTarget(fresh, version, name); err != nil {
			_ = fresh.Close()
			return nil, err
		}
		return fresh, nil
	}, agentRestartWaitOptions{})
	if err != nil {
		return nil, fmt.Errorf("reconnecting to updated simulator: %w", err)
	}
	defer next.Close()
	verified, err := verifyAgentAfterUpdate(ctx, next.AgentService, expectedHash, expectedVersion, agentVerifyWaitOptions{RequireHash: true})
	if err != nil {
		// The shared CLI verifier suggests an untargeted device update. Keep
		// recovery scoped to this simulator, even when a physical default exists.
		detail := strings.ReplaceAll(err.Error(), "re-run 'wendy device update'", "refresh this simulator's status and retry Finish setup")
		return nil, fmt.Errorf("verifying agent for vm:%s: %s", name, detail)
	}
	if err := requireRobotAgentCapability(ctx, next, profile.Kind); err != nil {
		return nil, fmt.Errorf("official agent %s was installed, but robot support is not available: %w", verified.Version, err)
	}
	result.Version, result.Updated, result.CapabilityVerified = verified.Version, true, true
	return result, nil
}

func verifySimulatorUpdateTarget(conn *grpcclient.AgentConnection, info *agentpb.GetAgentVersionResponse, name string) error {
	if conn == nil || conn.SimulatorName != name || info == nil ||
		(info.GetOs() != "linux" && info.GetOs() != "wendyos") ||
		info.GetDeviceType() != "vm-arm64" || info.GetCpuArchitecture() != "arm64" {
		return fmt.Errorf("agent update requires the verified ARM64 WendyOS VM %q", name)
	}
	return nil
}
