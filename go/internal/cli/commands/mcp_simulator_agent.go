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
	meta, _ := store.ReadMeta(name)
	if !meta.Arch.Valid() {
		return nil, fmt.Errorf("simulator %q records guest architecture %q, which this version of wendy cannot update", name, meta.Arch)
	}
	arch := meta.GuestArch()
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
	if err := verifySimulatorUpdateTarget(conn, info, name, arch); err != nil {
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
	reconnect := func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		fresh, version, err := simulatorUpdateConnect(ctx, name, address)
		if err != nil {
			return nil, err
		}
		if err := verifySimulatorUpdateTarget(fresh, version, name, arch); err != nil {
			_ = fresh.Close()
			return nil, err
		}
		return fresh, nil
	}
	next, err := waitForUpdatedAgentReady(ctx, reconnect, agentRestartWaitOptions{})
	if err != nil {
		return nil, fmt.Errorf("reconnecting to updated simulator: %w", err)
	}
	defer func() { next.Close() }()
	verified, err := verifyAgentAfterUpdate(ctx, next.AgentService, expectedHash, expectedVersion, agentVerifyWaitOptions{RequireHash: true})
	if err != nil {
		// The shared CLI verifier suggests an untargeted device update. Keep
		// recovery scoped to this simulator, even when a physical default exists.
		detail := strings.ReplaceAll(err.Error(), "re-run 'wendy device update'", "refresh this simulator's status and retry Finish setup")
		return nil, fmt.Errorf("verifying agent for vm:%s: %s", name, detail)
	}
	if _, next, err = awaitAgentVerdict(ctx, next, reconnect, expectedHash, func() {}); err != nil {
		return nil, fmt.Errorf("verifying agent for vm:%s: %w", name, err)
	}
	if err := requireRobotAgentCapability(ctx, next, profile.Kind); err != nil {
		return nil, fmt.Errorf("official agent %s was installed, but robot support is not available: %w", verified.Version, err)
	}
	result.Version, result.Updated, result.CapabilityVerified = verified.Version, true, true
	return result, nil
}

// verifySimulatorUpdateTarget requires the agent to be the named VM's, on the
// board and CPU architecture the VM was created with, before an agent built
// for that architecture is pushed to it.
func verifySimulatorUpdateTarget(conn *grpcclient.AgentConnection, info *agentpb.GetAgentVersionResponse, name string, arch vm.Arch) error {
	if arch == "" {
		arch = vm.ArchARM64
	}
	if conn == nil || conn.SimulatorName != name || !agentIsWendyOSVM(info) ||
		info.GetDeviceType() != arch.BoardID() || info.GetCpuArchitecture() != string(arch) {
		return fmt.Errorf("agent update requires the verified WendyOS VM %q", name)
	}
	return nil
}
