// Package discoverycache persists recently-seen LAN devices to
// ~/.wendy/devices.json so a subsequent `wendy device list`/picker can show
// them instantly while a live mDNS browse confirms and refreshes them in the
// background.
package discoverycache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TTL is how long a cached entry is considered fresh.
const TTL = time.Hour

// fileVersion is the on-disk schema version. A file with any other version
// is treated as corrupt (i.e. as an empty cache) rather than partially
// trusted.
const fileVersion = 1

// fileName is the cache file's name inside the wendy config directory.
const fileName = "devices.json"

// Entry is a cached, recently-seen LAN device.
type Entry struct {
	ID              string    `json:"id"`
	DisplayName     string    `json:"displayName"`
	Hostname        string    `json:"hostname"`
	IP              string    `json:"ip,omitempty"`
	Port            int       `json:"port"`
	MTLS            bool      `json:"mtls,omitempty"`
	AssetID         int32     `json:"assetId,omitempty"`
	OrgID           int32     `json:"orgId,omitempty"`
	MeshName        string    `json:"meshName,omitempty"`
	InterfaceName   string    `json:"interfaceName,omitempty"`
	WiredAddresses  []string  `json:"wiredAddresses,omitempty"`
	AgentVersion    string    `json:"agentVersion,omitempty"`
	DeviceType      string    `json:"deviceType,omitempty"`
	OS              string    `json:"os,omitempty"`
	OSVersion       string    `json:"osVersion,omitempty"`
	CPUArchitecture string    `json:"cpuArchitecture,omitempty"`
	LastSeen        time.Time `json:"lastSeen"`
}

// Key is the cache identity: lowercased id, falling back to lowercased
// displayName. Must match the picker's dedup identity (TXT device id,
// fallback display name).
func Key(id, displayName string) string {
	if id != "" {
		return strings.ToLower(id)
	}
	return strings.ToLower(displayName)
}

// Cache is a file-backed, in-memory set of recently-seen LAN devices. It is
// safe for concurrent use within a process; across processes, Flush merges
// with whatever is on disk at flush time.
type Cache struct {
	path    string
	mu      sync.Mutex
	entries map[string]Entry
	dirty   map[string]bool
}

// fileSchema is the on-disk representation of the cache file.
type fileSchema struct {
	Version int     `json:"version"`
	Devices []Entry `json:"devices"`
}

// Load opens ~/.wendy/devices.json. Missing, corrupt, or wrong-version files
// yield an empty cache and a nil error — the cache never fails a scan.
func Load() (*Cache, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	return LoadFrom(filepath.Join(dir, fileName))
}

// LoadFrom opens the cache file at path. Missing, corrupt, or wrong-version
// files yield an empty cache and a nil error — the cache never fails a scan.
func LoadFrom(path string) (*Cache, error) {
	c := &Cache{
		path:    path,
		entries: make(map[string]Entry),
		dirty:   make(map[string]bool),
	}
	for _, e := range readEntries(path) {
		c.entries[Key(e.ID, e.DisplayName)] = e
	}
	return c, nil
}

// readEntries reads and parses the cache file at path. A missing file,
// unreadable file, corrupt JSON, or unexpected version all yield a nil
// slice rather than an error: the cache is best-effort and must never block
// a scan.
func readEntries(path string) []Entry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var schema fileSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil
	}
	if schema.Version != fileVersion {
		return nil
	}
	return schema.Devices
}

// Fresh returns entries with LastSeen within TTL of now, any order.
func (c *Cache) Fresh(now time.Time) []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()

	fresh := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		if now.Sub(e.LastSeen) <= TTL {
			fresh = append(fresh, e)
		}
	}
	return fresh
}

// Entries returns every cached entry regardless of age, in any order. The
// connect fast path uses it — a stale IP is still worth one bounded dial
// attempt, with fallback to fresh resolution — while display surfaces
// (picker, discovery) keep using Fresh so the TTL still bounds what users
// see as "recently seen".
func (c *Cache) Entries() []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	return out
}

// Upsert merges e into the cache under Key(e.ID, e.DisplayName) and stamps
// LastSeen=now. Merge rule: a non-zero incoming field replaces the stored
// one; a zero incoming field keeps the stored value (so a browse-only upsert
// never wipes a probed AgentVersion).
func (c *Cache) Upsert(e Entry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := Key(e.ID, e.DisplayName)
	merged := mergeEntry(c.entries[key], e)
	merged.LastSeen = now
	c.entries[key] = merged
	c.dirty[key] = true
}

// Replace stores e verbatim under Key(e.ID, e.DisplayName), stamping
// LastSeen=now and discarding whatever was there. It is for callers that hold
// the device's complete current state — the streaming discovery engine, which
// already carries a probe's findings forward itself. For them Upsert's
// non-zero-wins merge would only resurrect values the device has since
// dropped, such as an mTLS flag or an orgid it stopped advertising.
func (c *Cache) Replace(e Entry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e.LastSeen = now
	key := Key(e.ID, e.DisplayName)
	c.entries[key] = e
	c.dirty[key] = true
}

// Delete removes the entry stored under key and records the removal so the
// next Flush drops it from the file too (rather than re-reading it off disk
// and keeping it). Deleting an absent key is a no-op on this cache but still
// removes whatever is on disk under that key — which is the point: the caller
// is the streaming engine retiring an identity it has proven stale, e.g. a
// connect-minted hostname row superseded by the device's real TXT device id.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
	c.dirty[key] = true
}

// mergeEntry applies incoming's non-zero fields on top of stored, leaving
// stored's value wherever incoming carries the zero value for that field.
func mergeEntry(stored, incoming Entry) Entry {
	result := stored
	if incoming.ID != "" {
		result.ID = incoming.ID
	}
	if incoming.DisplayName != "" {
		result.DisplayName = incoming.DisplayName
	}
	if incoming.Hostname != "" {
		result.Hostname = incoming.Hostname
	}
	if incoming.IP != "" {
		result.IP = incoming.IP
	}
	if incoming.Port != 0 {
		result.Port = incoming.Port
	}
	if incoming.MTLS {
		result.MTLS = incoming.MTLS
	}
	if incoming.AssetID != 0 {
		result.AssetID = incoming.AssetID
	}
	if incoming.OrgID != 0 {
		result.OrgID = incoming.OrgID
	}
	if incoming.MeshName != "" {
		result.MeshName = incoming.MeshName
	}
	if incoming.InterfaceName != "" {
		result.InterfaceName = incoming.InterfaceName
	}
	if len(incoming.WiredAddresses) > 0 {
		result.WiredAddresses = incoming.WiredAddresses
	}
	if incoming.AgentVersion != "" {
		result.AgentVersion = incoming.AgentVersion
	}
	if incoming.DeviceType != "" {
		result.DeviceType = incoming.DeviceType
	}
	if incoming.OS != "" {
		result.OS = incoming.OS
	}
	if incoming.OSVersion != "" {
		result.OSVersion = incoming.OSVersion
	}
	if incoming.CPUArchitecture != "" {
		result.CPUArchitecture = incoming.CPUArchitecture
	}
	return result
}

// Flush persists: re-reads the file, overlays this cache's dirty entries
// (a dirty key this cache no longer holds was deleted, so it is dropped from
// the file too), drops entries older than TTL, writes temp file + atomic
// os.Rename. Concurrent CLIs: last writer wins, lost writes are re-learned
// next scan.
func (c *Cache) Flush(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	merged := make(map[string]Entry)
	for _, e := range readEntries(c.path) {
		merged[Key(e.ID, e.DisplayName)] = e
	}
	for key := range c.dirty {
		if e, ok := c.entries[key]; ok {
			merged[key] = e
		} else {
			delete(merged, key)
		}
	}

	kept := make(map[string]Entry, len(merged))
	devices := make([]Entry, 0, len(merged))
	for key, e := range merged {
		if now.Sub(e.LastSeen) > TTL {
			continue
		}
		kept[key] = e
		devices = append(devices, e)
	}

	data, err := json.MarshalIndent(fileSchema{Version: fileVersion, Devices: devices}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling device cache: %w", err)
	}

	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".devices-*.json")
	if err != nil {
		return fmt.Errorf("creating temp device cache file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp device cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp device cache file: %w", err)
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp device cache file: %w", err)
	}

	c.entries = kept
	c.dirty = make(map[string]bool)
	return nil
}

// EntryFromDevice converts a discovered LANDevice into a cache Entry.
// LastSeen is left zero; callers stamp it via Upsert.
func EntryFromDevice(dev models.LANDevice) Entry {
	return Entry{
		ID:              dev.ID,
		DisplayName:     dev.DisplayName,
		Hostname:        dev.Hostname,
		IP:              dev.IPAddress,
		Port:            dev.Port,
		MTLS:            dev.IsMTLS,
		AssetID:         dev.AssetID,
		OrgID:           dev.OrgID,
		MeshName:        dev.MeshName,
		InterfaceName:   dev.NetworkInterface,
		WiredAddresses:  dev.WiredAddresses,
		AgentVersion:    dev.AgentVersion,
		DeviceType:      dev.DeviceType,
		OS:              dev.OS,
		OSVersion:       dev.OSVersion,
		CPUArchitecture: dev.CPUArchitecture,
	}
}

// Device converts an Entry back into a models.LANDevice for use in the
// discovery pipeline. InterfaceType and IsWendyDevice are reconstructed
// (a cached entry is always LAN and always a confirmed Wendy device);
// NetworkInterface and IsMTLS round-trip from InterfaceName and MTLS.
func (e Entry) Device() models.LANDevice {
	return models.LANDevice{
		ID:               e.ID,
		DisplayName:      e.DisplayName,
		Hostname:         e.Hostname,
		IPAddress:        e.IP,
		Port:             e.Port,
		IsMTLS:           e.MTLS,
		AssetID:          e.AssetID,
		OrgID:            e.OrgID,
		MeshName:         e.MeshName,
		InterfaceType:    string(models.InterfaceLAN),
		NetworkInterface: e.InterfaceName,
		WiredAddresses:   e.WiredAddresses,
		IsWendyDevice:    true,
		AgentVersion:     e.AgentVersion,
		DeviceType:       e.DeviceType,
		OS:               e.OS,
		OSVersion:        e.OSVersion,
		CPUArchitecture:  e.CPUArchitecture,
	}
}
