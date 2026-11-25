package localmesh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// BootClock is a mesh-only certificate clock. Local historical anchors are
// lower bounds, not fresh signed UTC; this object never writes the system clock
// or gives an announcement's timestamps authority over certificate validation.
type BootClock struct {
	mu           sync.Mutex
	now          func() time.Time
	boot         func() (int64, error)
	anchor       time.Time
	anchorBoot   int64
	progressed   time.Time
	source       string
	capturedWall time.Time
	capturedBoot int64
	configFloor  time.Time
	certain      bool
}

type BootClockSnapshot struct {
	Wall, Base, Anchor, ConfigFloor time.Time
	CapturedWall                    time.Time
	CapturedBootNanos               int64
	BootNanos                       int64
	Source                          string
	Uncertain                       bool
}

// NewBootClock selects only locally installed time anchors. The config floor
// uses the same encoded seconds and plausibility interval as timesync.ApplyFloor;
// /config is a fallback for images where its private copy is not yet present.
func NewBootClock(configPath string) (*BootClock, error) {
	return newBootClock(configPath, "/config", "/var/lib/systemd/timesync/clock", time.Now, timesync.BootTime)
}

func newBootClock(configPath, fallbackPath, savedPath string, now func() time.Time, boot func() (int64, error)) (*BootClock, error) {
	c := &BootClock{now: now, boot: boot}
	before, err := boot()
	if err != nil {
		return nil, fmt.Errorf("mesh boot clock unavailable: %w", err)
	}
	if before < 0 {
		return nil, errors.New("mesh boot clock is negative")
	}
	wall := now()
	floor := readBootClockFloor(filepath.Join(configPath, "clock_floor"))
	c.configFloor = floor
	source := "config-floor"
	if floor.IsZero() {
		floor = readBootClockFloor(filepath.Join(fallbackPath, "clock_floor"))
		source = "config-partition-floor"
	}
	if !floor.IsZero() {
		c.anchor = floor
		c.source = source
	}
	if info, err := os.Stat(savedPath); err == nil && info.Mode().IsRegular() && plausibleBootAnchor(info.ModTime()) && info.ModTime().After(c.anchor) {
		c.anchor = info.ModTime()
		c.source = "systemd-saved-clock"
	}
	after, err := boot()
	if err != nil || after < before {
		return nil, errors.New("mesh boot clock changed during local anchor capture")
	}
	c.anchorBoot = after
	c.progressed = c.anchor
	// Capture UTC between boot samples for diagnostics only. Wall time never
	// clears explicit boot uncertainty, even if it is ahead of every anchor.
	c.capturedWall = wall
	c.capturedBoot = after
	return c, nil
}

func plausibleBootAnchor(t time.Time) bool {
	return !t.Before(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) && !t.After(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
}
func readBootClockFloor(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	var data [8]byte
	if _, err = io.ReadFull(f, data[:]); err != nil {
		return time.Time{}
	}
	floor := time.Unix(int64(binary.BigEndian.Uint64(data[:])), 0)
	if !plausibleBootAnchor(floor) {
		return time.Time{}
	}
	return floor
}

// AddProvisionedCertificate contributes the locally provisioned signing leaf's
// NotBefore, never a peer certificate. Credential construction still verifies
// the complete chain, identity, role and matching key before any mesh use.
func (c *BootClock) AddProvisionedCertificate(certPEM string) error {
	leaves, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil {
		return err
	}
	if len(leaves) == 0 {
		return errors.New("mesh boot clock missing provisioned certificate")
	}
	before, err := c.boot()
	if err != nil {
		return err
	}
	anchor := leaves[0].NotBefore
	after, err := c.boot()
	if err != nil || after < before {
		return errors.New("mesh boot clock changed during certificate capture")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.floorAtLocked(after)
	if anchor.After(current) {
		c.anchor = anchor
		c.progressed = anchor
		c.anchorBoot = after
		c.source = "provisioned-certificate"
	}
	return nil
}

func (c *BootClock) floorAtLocked(boot int64) time.Time {
	if !c.anchor.IsZero() && boot >= c.anchorBoot {
		if progressed := c.anchor.Add(time.Duration(boot - c.anchorBoot)); progressed.After(c.progressed) {
			c.progressed = progressed
		}
	}
	return c.progressed
}
func (c *BootClock) floor() time.Time {
	boot, err := c.boot()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		return c.progressed
	}
	return c.floorAtLocked(boot)
}

func (c *BootClock) Policy() certs.CertificateTimePolicy {
	return certs.CertificateTimePolicy{Now: c.now, Floor: c.floor, Uncertain: func() bool { c.mu.Lock(); defer c.mu.Unlock(); return !c.certain }}
}

// ObserveDirectSync withdraws boot-only NotBefore grace only after the normal
// direct signed-quorum path proves that actual UTC is inside its interval.
// Advance-only success while UTC is already ahead is deliberately insufficient.
func (c *BootClock) ObserveDirectSync(o timesync.DirectSyncObservation) {
	if o.Source != "direct" || o.Quorum < 2 || !o.UTCWithinVerifiedInterval || o.ApplyError != nil || o.ObservationError != nil {
		return
	}
	c.mu.Lock()
	c.certain = true
	c.mu.Unlock()
}

func (c *BootClock) Snapshot() BootClockSnapshot {
	boot, err := c.boot()
	wall := c.now()
	if after, afterErr := c.boot(); afterErr == nil && after >= boot {
		boot = after
	} else {
		err = errors.New("mesh boot clock changed during snapshot")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	floor := c.progressed
	if err == nil {
		floor = c.floorAtLocked(boot)
	}
	base := wall
	if floor.After(base) {
		base = floor
	}
	return BootClockSnapshot{Wall: wall, Base: base, Anchor: floor, ConfigFloor: c.configFloor, BootNanos: boot, Source: c.source, Uncertain: !c.certain, CapturedWall: c.capturedWall, CapturedBootNanos: c.capturedBoot}
}
