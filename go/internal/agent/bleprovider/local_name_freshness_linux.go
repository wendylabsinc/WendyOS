//go:build linux

package bleprovider

import (
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	// Correlation is deliberately much shorter than candidate freshness.
	// Both parts must be live; a repeated name cannot renew an old tenant UUID.
	scanResponseCorrelationTTL   = 3 * time.Second
	maxAdvertisementObservations = 256
)

type localNameKey struct {
	path        dbus.ObjectPath
	addressType string
}

type localNameParts struct {
	lastSeen   time.Time
	tenantSeen time.Time
	nameSeen   time.Time
	advert     Advertisement
}

func recentPart(seen, now time.Time) bool {
	return !seen.IsZero() && !now.Before(seen) && now.Sub(seen) < scanResponseCorrelationTTL
}

// observeReport accepts only live management reports from the selected adapter.
// Source attribution matters: unrelated name/RSSI updates must never resurrect
// a cached announcement, and malformed binary data must not select a text fallback.
func (f *advertisementFreshness) observeReport(address, addressType string, data []byte, at time.Time) {
	if _, _, err := parseAddress(address, addressType, DefaultPSM); err != nil {
		return
	}
	path := dbus.ObjectPath(string(f.adapter) + "/dev_" + strings.ReplaceAll(strings.ToUpper(address), ":", "_"))
	fields, err := parseAdvertisingData(data, f.uuid)
	if err != nil {
		return
	}
	if fields.uuidListPresent && !fields.tenant && !fields.servicePresent {
		return // Another service's announcement cannot supply our name part.
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.seen[path]; ok && at.Before(old.seen) {
		return
	}
	if fields.servicePresent {
		adv, err := ParseAdvertisement(fields.service, f.networkName)
		if err != nil {
			delete(f.seen, path)
			for key := range f.partial {
				if key.path == path {
					delete(f.partial, key)
				}
			}
			return
		}
		f.recordReportLocked(path, adv, at, false, address, addressType)
		return
	}
	if !fields.tenant && !fields.namePresent {
		return
	}
	key := localNameKey{path, addressType}
	if old, ok := f.partial[key]; ok && at.Before(old.lastSeen) {
		return
	}
	// Bound partial state even in a busy public radio environment.
	for key, parts := range f.partial {
		if !at.Before(parts.lastSeen) && !recentPart(parts.tenantSeen, at) && !recentPart(parts.nameSeen, at) {
			delete(f.partial, key)
		}
	}
	parts, exists := f.partial[key]
	if !exists && len(f.partial) >= maxAdvertisementObservations {
		return
	}
	if fields.tenant && !at.Before(parts.tenantSeen) {
		parts.tenantSeen = at
	}
	// A shortened name is never a payload. Retain the live tenant UUID from
	// that primary packet so a subsequent complete scan response can join it.
	if fields.namePresent && !fields.shortened && !at.Before(parts.nameSeen) {
		adv, err := ParseLocalName(fields.name, f.networkName)
		if err != nil {
			if !fields.tenant {
				// Other apps can advertise different names on the same radio.
				// Without our UUID in this report, ignore rather than withdraw
				// or refresh an earlier valid mesh announcement.
				return
			}
			for key := range f.partial {
				if key.path == path {
					delete(f.partial, key)
				}
			}
			if f.seen[path].fromName {
				delete(f.seen, path)
			}
			return
		}
		parts.nameSeen, parts.advert = at, adv
	}
	parts.lastSeen = at
	f.partial[key] = parts
	if !recentPart(parts.tenantSeen, at) || !recentPart(parts.nameSeen, at) {
		return
	}
	if old, ok := f.seen[path]; ok && !old.fromName && !at.Before(old.seen) && at.Sub(old.seen) < advertisementFreshnessTTL {
		return
	}
	// The older component determines expiry, not the newest packet or cache poll.
	seen := parts.tenantSeen
	if parts.nameSeen.Before(seen) {
		seen = parts.nameSeen
	}
	f.recordReportLocked(path, parts.advert, seen, true, address, addressType)
}

func (f *advertisementFreshness) recordReportLocked(path dbus.ObjectPath, adv Advertisement, at time.Time, fromName bool, address, addressType string) {
	f.recordLocked(path, adv, at, fromName)
	entry := f.seen[path]
	if entry.advert != adv || !entry.seen.Equal(at) || entry.fromName != fromName {
		return
	}
	entry.address, entry.addressType = address, addressType
	f.seen[path] = entry
}

// recordLocked requires f.mu. Separate radio addresses with equal prefixes are
// retained independently until TLS resolves their complete principals.
func (f *advertisementFreshness) recordLocked(path dbus.ObjectPath, adv Advertisement, at time.Time, fromName bool) {
	if old, ok := f.seen[path]; ok && at.Before(old.seen) {
		return
	}
	for priorPath, entry := range f.seen {
		if !at.Before(entry.seen) && at.Sub(entry.seen) >= advertisementFreshnessTTL {
			delete(f.seen, priorPath)
		}
	}
	if _, exists := f.seen[path]; !exists && len(f.seen) >= maxFreshAdvertisements {
		return
	}
	f.seen[path] = freshAdvertisement{advert: adv, fromName: fromName, seen: at, pending: true}
}

func (f *advertisementFreshness) clearReports() {
	f.mu.Lock()
	clear(f.partial)
	for path, entry := range f.seen {
		if entry.fromName {
			delete(f.seen, path)
		}
	}
	f.mu.Unlock()
}
