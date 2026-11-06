package ipcam

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type leaseFile struct {
	Version int          `json:"version"`
	BootID  string       `json:"bootId,omitempty"`
	Leases  []savedLease `json:"leases"`
}

func openLeasePool(path string, seg CameraSegment, legacy []Camera) (*LeasePool, error) {
	bootID, err := leaseBootID()
	if err != nil {
		return nil, fmt.Errorf("reading DHCP lease boot identity: %w", err)
	}
	return openLeasePoolWithClock(path, seg, legacy, bootID, leaseBootTime)
}

// Persisted deadlines use boot elapsed time, never the adjustable wall clock.
// A different boot cannot establish how much of a client's lease remains, so
// reserve old addresses until this boot is twelve hours old. Save the rebased
// deadlines before serving; agent restarts in this boot must not extend them.
func openLeasePoolWithClock(path string, seg CameraSegment, legacy []Camera, bootID string, clock func() (time.Duration, error)) (*LeasePool, error) {
	if bootID == "" {
		return nil, fmt.Errorf("empty DHCP lease boot identity")
	}
	p := NewLeasePool(seg.PoolBase, seg.PoolSize)
	p.now = clock
	p.save = func(records []savedLease) error { return saveLeaseFile(path, bootID, records) }
	now, err := p.now()
	if err != nil {
		return nil, fmt.Errorf("reading DHCP lease clock: %w", err)
	}
	saved, missing, err := readLeaseFile(path)
	if err != nil {
		return nil, err
	}
	if missing {
		saved = leaseFile{Version: 2, BootID: bootID, Leases: migrationReservations(p, legacy, now+LeaseWindow)}
	}
	changedBoot := saved.BootID != bootID
	seenMAC, seenAddr := make(map[string]bool), make(map[string]bool)
	for _, record := range saved.Leases {
		ip := net.ParseIP(record.Address)
		mac, macErr := net.ParseMAC(record.MAC)
		if !p.contains(ip) || (record.Quarantined && record.MAC != "") || (!record.Quarantined && (macErr != nil || len(mac) != 6)) {
			return nil, fmt.Errorf("invalid camera DHCP lease in %s", path)
		}
		expiry := record.ExpiresAfterBoot
		if saved.Version == 1 {
			if record.LegacyExpiresAt == nil || record.LegacyExpiresAt.IsZero() || record.Quarantined {
				return nil, fmt.Errorf("invalid legacy camera DHCP lease in %s", path)
			}
			// Version 1 can have been written earlier in this same boot before
			// a clock correction. Its wall timestamp proves no elapsed time.
			expiry = now + LeaseWindow
		} else {
			if expiry <= 0 {
				return nil, fmt.Errorf("invalid camera DHCP deadline in %s", path)
			}
			if changedBoot {
				expiry = LeaseWindow
			}
		}
		key, address := mac.String(), ip.String()
		if seenAddr[address] || (!record.Quarantined && seenMAC[key]) {
			return nil, fmt.Errorf("duplicate camera DHCP lease in %s", path)
		}
		seenAddr[address] = true
		if !record.Quarantined {
			seenMAC[key] = true
		}
		if expiry <= now {
			continue
		}
		if record.Quarantined {
			p.quarantined[address], p.byAddr[address] = expiry, ""
		} else {
			p.byMAC[key], p.byAddr[address], p.expires[key] = ip.To4(), key, expiry
		}
	}
	if missing || changedBoot || saved.Version == 1 {
		if err := p.save(p.recordsLocked("")); err != nil {
			return nil, fmt.Errorf("initializing camera DHCP leases: %w", err)
		}
	}
	return p, nil
}

// Registry addresses are historical observations, not a bijective lease table.
// Quarantine ambiguous addresses for one window; assigning either historical
// owner could collide with the other. Unique observations keep their MAC binding.
func migrationReservations(p *LeasePool, cameras []Camera, expiry time.Duration) []savedLease {
	owners := make(map[string]map[string]bool)
	addresses := make(map[string]map[string]bool)
	for _, camera := range cameras {
		ip := net.ParseIP(camera.Address)
		if !p.contains(ip) {
			continue
		}
		mac, err := net.ParseMAC(camera.MAC)
		key := ""
		if err == nil && len(mac) == 6 {
			key = mac.String()
		}
		address := ip.String()
		if owners[address] == nil {
			owners[address] = make(map[string]bool)
		}
		if addresses[key] == nil {
			addresses[key] = make(map[string]bool)
		}
		owners[address][key], addresses[key][address] = true, true
	}
	var records []savedLease
	for address, candidates := range owners {
		record := savedLease{Address: address, ExpiresAfterBoot: expiry, Quarantined: true}
		if len(candidates) == 1 {
			for mac := range candidates {
				if mac != "" && len(addresses[mac]) == 1 {
					record.MAC, record.Quarantined = mac, false
				}
			}
		}
		records = append(records, record)
	}
	return records
}

func readLeaseFile(path string) (leaseFile, bool, error) {
	var saved leaseFile
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return saved, true, nil
	}
	if err != nil {
		return saved, false, err
	}
	if err := rootOwnedUnwritable(info, false); err != nil {
		return saved, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return saved, false, err
	}
	defer f.Close()
	const maxSize = 64 * 1024
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return saved, false, err
	}
	if len(data) > maxSize {
		return saved, false, fmt.Errorf("camera DHCP lease file exceeds %d bytes", maxSize)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return saved, false, fmt.Errorf("reading camera DHCP leases: %w", err)
	}
	if saved.Version != 1 && saved.Version != 2 {
		return saved, false, fmt.Errorf("unsupported camera DHCP lease version %d", saved.Version)
	}
	if saved.Version == 2 && saved.BootID == "" {
		return saved, false, fmt.Errorf("missing camera DHCP boot identity")
	}
	return saved, false, nil
}

func saveLeaseFile(path, bootID string, records []savedLease) error {
	dir := filepath.Dir(path)
	// This is the existing, validated agent configuration directory. Do not
	// create an untracked directory whose entry might be lost on power failure.
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := rootOwnedUnwritable(info, true); err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Address < records[j].Address })
	data, err := json.Marshal(leaseFile{Version: 2, BootID: bootID, Leases: records})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".camera-dhcp-leases-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDHCPDirectory(dir)
}
