package containerd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// meshRetirementIP reads the embedded host-local allocator's exact container
// ownership before any DEL or admission revocation can discard that evidence.
// A CNI result on its own is not authority: siblings share an app subnet and a
// stale result must never remove the current owner's rules. An allocation is
// kept pinned when its proof is inconsistent, rather than released for reuse.
func meshRetirementIP(containerID, appID, cachedIP string) (string, error) {
	raw, err := os.ReadFile(cniSubnetRegistryPath)
	_, proofErr := os.Stat(networkSandboxResultPath(containerID))
	_, stateErr := os.Stat(filepath.Join(filepath.Dir(cniSubnetRegistryPath), appID))
	fresh := cachedIP == "" && errors.Is(proofErr, os.ErrNotExist) && errors.Is(stateErr, os.ErrNotExist)
	if errors.Is(err, os.ErrNotExist) && fresh {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading net subnet ownership: %w", err)
	}
	var registry map[string]string
	if err := json.Unmarshal(raw, &registry); err != nil {
		return "", err
	}
	subnet, exists := registry[appID]
	if !exists {
		if fresh {
			return "", nil
		}
		return "", fmt.Errorf("net app %q has no trusted subnet allocation", appID)
	}
	_, prefix, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", err
	}
	// The same backend and network/dataDir used by buildBridgeCNIConfig provide
	// the reverse lookup; do not parse host-local's private file format ourselves.
	store, err := disk.New("wendy-"+appID, filepath.Join(filepath.Dir(cniSubnetRegistryPath), appID))
	if err != nil {
		return "", err
	}
	defer store.Close()
	if err := store.Lock(); err != nil {
		return "", err
	}
	defer store.Unlock()
	// GetByID intentionally tolerates I/O failures for best-effort CNI DEL.
	// Retirement cannot treat an unreadable lease as proof of no owner.
	networkDir := filepath.Join(filepath.Dir(cniSubnetRegistryPath), appID, "wendy-"+appID)
	entries, err := os.ReadDir(networkDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if net.ParseIP(entry.Name()) == nil {
			continue
		}
		if !entry.Type().IsRegular() {
			return "", fmt.Errorf("invalid net IPAM lease %q", entry.Name())
		}
		if _, err := os.ReadFile(filepath.Join(networkDir, entry.Name())); err != nil {
			return "", err
		}
	}
	ips := store.GetByID(containerID, "eth0")
	if len(ips) == 0 {
		if cachedIP != "" {
			return "", fmt.Errorf("net app %q has a cached address without IPAM ownership", containerID)
		}
		if _, err := os.Stat(networkSandboxResultPath(containerID)); !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("net app %q has CNI proof without IPAM ownership", containerID)
		}
		return "", nil
	}
	if len(ips) != 1 || ips[0].To4() == nil || !prefix.Contains(ips[0]) {
		return "", fmt.Errorf("net app %q has ambiguous IPAM ownership", containerID)
	}
	ip := ips[0].String()
	if cachedIP != "" && cachedIP != ip {
		return "", fmt.Errorf("net app %q IP cache does not match IPAM ownership", containerID)
	}
	if result, err := readNetworkSandboxResult(containerID); err == nil {
		recorded, err := verifiedMeshResultIP(result, subnet)
		if err != nil || recorded != ip {
			return "", fmt.Errorf("net app %q has inconsistent CNI address proof", containerID)
		}
		if _, err := meshCheckResultForTask(result, networkSandboxPath(containerID), networkSandboxPath(containerID)); err != nil {
			return "", fmt.Errorf("net app %q has untrusted CNI namespace proof: %w", containerID, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return ip, nil
}

// retireMeshNetwork must precede CNI DEL, task replacement, and proof cleanup.
// Callers hold the concrete container lifecycle lock. cachedIP is copied while
// c.mu is held; a recovered task may instead be proven by its pinned IPAM lease.
// Failure revokes admission but preserves the allocation and proof for retry.
func (c *Client) retireMeshNetwork(containerID string, labels map[string]string, cachedIP string) error {
	entitlements := parseEntitlementsFromAnnotations(labels)
	if _, ok := findMeshEntitlement(entitlements); !ok {
		return nil
	}
	appID, service := labels[labelKeyAppID], labels[labelKeyServiceName]
	if appconfig.ValidateAppID(appID) != nil || (service != "" && appconfig.ValidateServiceName(service) != nil) || ContainerName(appID, service) != containerID {
		c.releaseMeshIngress(containerID)
		return fmt.Errorf("untrusted net container ownership for %q", containerID)
	}
	ip, err := meshRetirementIP(containerID, appID, cachedIP)
	if err != nil {
		c.releaseMeshIngress(containerID)
		return err
	}
	return c.teardownMeshEgress(entitlements, containerID, appID, ip)
}

// forgetRetiredMeshIP drops the cache only after checked rule retirement has
// succeeded. Callers hold c.mu and their concrete container lifecycle lock.
// This makes stop followed by delete idempotent without mistaking a cached
// live address whose lease vanished for an already-retired allocation.
func (c *Client) forgetRetiredMeshIP(labels map[string]string) {
	if _, meshApp := findMeshEntitlement(parseEntitlementsFromAnnotations(labels)); meshApp {
		delete(c.serviceIPs[labels[labelKeyAppID]], labels[labelKeyServiceName])
	}
}
