package ipcam

import (
	"fmt"
	"net"
	"os"
)

// MigrateLegacyDHCPAllowlist preserves camera links served before explicit
// opt-in was required. Call after LoadDHCPAllowlist and before starting discovery.
// Any existing configuration, including an empty file written by disable, is
// authoritative. Creating the file even with no candidates completes migration;
// subsequent startups must never infer permission again from historical records.
func (m *LinkManager) MigrateLegacyDHCPAllowlist() error {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	path := m.allowlistPath
	m.mu.Unlock()
	if path == "" || m.reg == nil {
		return nil
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return err
	}

	// Read the persisted registry independently: a failed startup load must not
	// look like an empty registry and permanently complete migration. There is
	// no recency cutoff; an intermediate opt-in agent may have left these cameras
	// unreachable for days without updating their records.
	legacy := NewRegistry(m.reg.path)
	if err := legacy.Load(); err != nil {
		return fmt.Errorf("loading legacy camera DHCP records: %w", err)
	}
	allowed := legacyDHCPInterfaces(legacy.List())
	if err := createDHCPAllowlist(path, allowed); err != nil && !os.IsExist(err) {
		return fmt.Errorf("saving migrated camera DHCP configuration: %w", err)
	}
	// A concurrent operator-created file takes precedence over the candidates.
	if err := m.refreshDHCPAllowlist(); err != nil {
		return err
	}
	m.logger.Info("completed legacy camera DHCP configuration migration")
	return nil
}

func legacyDHCPInterfaces(cameras []Camera) map[string]bool {
	allowed := make(map[string]bool)
	for _, cam := range cameras {
		if !validDHCPInterfaceName(cam.Link) {
			continue
		}
		ip := net.ParseIP(cam.Address).To4()
		index, ok := SegmentIndexFor(ip)
		if !ok {
			continue
		}
		seg := SegmentFor(index)
		base := seg.PoolBase.To4()[3]
		if ip[3] >= base && int(ip[3])-int(base) < seg.PoolSize {
			// Older genuine leases lack DHCPServed. The saved port and exact
			// 10.98.n.50-149 pool are the legacy signal; normal live safety
			// checks still decide whether an enabled port may actually serve.
			allowed[cam.Link] = true
		}
	}
	return allowed
}
