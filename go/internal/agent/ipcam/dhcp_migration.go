package ipcam

import (
	"fmt"
	"net"
	"os"
)

// MigrateLegacyDHCPAllowlist preserves camera links served before explicit
// opt-in was required, but only for MACs with stored camera credentials. Call
// after LoadDHCPAllowlist and before starting discovery.
// Any existing configuration, including an empty file written by disable, is
// authoritative. Creating the file even with no candidates completes migration;
// subsequent startups must never infer permission again from historical records.
func (m *LinkManager) MigrateLegacyDHCPAllowlist(credentials *CredentialStore) error {
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
	if credentials == nil {
		return fmt.Errorf("legacy camera DHCP migration requires a credential store")
	}

	// Read the persisted registry independently: a failed startup load must not
	// look like an empty registry and permanently complete migration. There is
	// no recency cutoff; an intermediate opt-in agent may have left these cameras
	// unreachable for days without updating their records.
	legacy := NewRegistry(m.reg.path)
	if err := legacy.Load(); err != nil {
		return fmt.Errorf("loading legacy camera DHCP records: %w", err)
	}
	// Reload credentials too: a failed startup load must not be mistaken for
	// an empty store and permanently exclude configured cameras from migration.
	paired := NewCredentialStore(credentials.path)
	if err := paired.Load(); err != nil {
		return fmt.Errorf("loading legacy camera credentials: %w", err)
	}
	allowed := legacyDHCPInterfaces(legacy.List(), paired)
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

func legacyDHCPInterfaces(cameras []Camera, credentials *CredentialStore) map[string]bool {
	allowed := make(map[string]bool)
	if credentials == nil {
		return allowed
	}
	for _, cam := range cameras {
		if cam.MAC == "" || !credentials.Has(cam.MAC) || !validDHCPInterfaceName(cam.Link) {
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
			// Older genuine leases lack DHCPServed. A MAC-matched login
			// establishes operator intent; an old lease alone could belong to
			// an ordinary LAN client the agent mistakenly served. Normal live
			// safety checks still decide whether an enabled port may serve.
			allowed[cam.Link] = true
		}
	}
	return allowed
}
