//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// driveIdentity fingerprints a freshly imaged drive so a provisioning retry can
// find it again after it drops off the bus and re-enumerates — possibly under a
// different /dev/diskN or PhysicalDriveN (WDY-3974: a Realtek RTL9210C NVMe
// enclosure reset mid-write and came back only after a replug).
//
// The GUIDs come from the image just written, so they tell this drive apart
// from any disk that does not carry it, but not from another drive written
// from the same image: size, serial and model narrow that down, and
// findReconnectedDrive refuses outright when more than one drive still
// matches. Fields an OS cannot report without elevation stay empty — macOS
// exposes partition GUIDs but not the GPT disk GUID, and Windows cannot read
// volume labels on the offline disk, so it has no config-partition GUID.
type driveIdentity struct {
	SizeBytes int64
	// DiskGUID is the GPT disk GUID (Linux PTUUID, Windows Get-Disk Guid).
	DiskGUID string
	// ConfigPartitionGUID is the unique GUID of the partition labelled "config".
	ConfigPartitionGUID string
	Serial              string
	Model               string
}

// usable reports whether id can positively identify a drive: it needs the
// size and at least one GUID from the written image.
func (id driveIdentity) usable() bool {
	return id.SizeBytes > 0 && (id.DiskGUID != "" || id.ConfigPartitionGUID != "")
}

// matches reports whether other is the drive id was taken from: the same size,
// and every field id recorded agrees. A field other lacks never matches one id
// has, so a drive that is still enumerating is not mistaken for ours.
func (id driveIdentity) matches(other driveIdentity) bool {
	if !id.usable() || other.SizeBytes != id.SizeBytes {
		return false
	}
	for _, f := range [][2]string{
		{id.DiskGUID, other.DiskGUID},
		{id.ConfigPartitionGUID, other.ConfigPartitionGUID},
		{id.Serial, other.Serial},
		{id.Model, other.Model},
	} {
		if f[0] != "" && !strings.EqualFold(f[0], f[1]) {
			return false
		}
	}
	return true
}

// normalizeGUID strips the braces Windows puts around GUIDs and lowercases
// them, so the same GUID compares equal whichever tool reported it.
func normalizeGUID(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), "{}"))
}

// identifiedDrive is an attached drive together with its fingerprint.
type identifiedDrive struct {
	drive drive
	id    driveIdentity
}

// The disk lookups behind the reconnect flow are package vars so tests can
// fake them without real disks.
var (
	readDriveIdentityFn    = readDriveIdentity
	listIdentifiedDrivesFn = listIdentifiedDrives
	drivePresentFn         = drivePresent
	reconnectSleep         = sleepCtx
	reconnectTimeout       = 2 * time.Minute
)

// reconnectPollInterval is how often a reconnect wait re-lists the drives.
const reconnectPollInterval = 2 * time.Second

// sleepCtx waits for d, or until ctx is cancelled (Ctrl-C).
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// replugGuidance tells the user how to bring a disconnected drive back.
const replugGuidance = "Unplug the drive and plug it back in. On retry, wendy waits for it to reappear and checks it is the drive it just imaged before writing anything."

var (
	// errDriveNotReconnected means no attached drive matches the imaged
	// drive's identity (yet).
	errDriveNotReconnected = errors.New("the drive did not reconnect")
	// errDriveAmbiguous means more than one attached drive matches it.
	errDriveAmbiguous = errors.New("more than one drive matches the drive just written")
)

// driveDisconnected reports whether a provisioning failure came from d dropping
// off the bus — a USB enclosure resetting, an SD card unseated — so retrying
// against its device path would fail the same way: the write failed with
// ENXIO/ENODEV ("device not configured" on macOS, which the elevated cp
// reports only as text), an earlier reconnect wait came up empty, or d's
// device node is gone.
func driveDisconnected(d drive, err error) bool {
	if errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENODEV) || errors.Is(err, errDriveNotReconnected) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "device not configured") || strings.Contains(msg, "no such device or address") {
		return true
	}
	return !drivePresentFn(d)
}

// awaitReconnectedDrive polls for up to reconnectTimeout for the drive id was
// taken from to come back. It returns a drive only once exactly one attached
// drive matches id, and never falls back to the old device path: after a
// disconnect that path may name a different disk.
func awaitReconnectedDrive(ctx context.Context, id driveIdentity) (drive, error) {
	fmt.Printf("Waiting up to %s for the drive to reconnect...\n", reconnectTimeout)
	deadline := time.Now().Add(reconnectTimeout)
	for {
		found, err := findReconnectedDrive(id)
		if err == nil {
			fmt.Printf("Found the drive again at %s.\n", found.DevicePath)
			return found, nil
		}
		if !errors.Is(err, errDriveNotReconnected) {
			return drive{}, err
		}
		if !time.Now().Add(reconnectPollInterval).Before(deadline) {
			return drive{}, fmt.Errorf("%w within %s", errDriveNotReconnected, reconnectTimeout)
		}
		if err := reconnectSleep(ctx, reconnectPollInterval); err != nil {
			return drive{}, err
		}
	}
}

// findReconnectedDrive returns the one attached drive that matches id.
func findReconnectedDrive(id driveIdentity) (drive, error) {
	candidates, err := listIdentifiedDrivesFn()
	if err != nil {
		// Listing can fail while the bus re-enumerates; that is "not back yet".
		return drive{}, fmt.Errorf("%w (listing drives: %v)", errDriveNotReconnected, err)
	}
	var matches []drive
	for _, c := range candidates {
		if id.matches(c.id) {
			matches = append(matches, c.drive)
		}
	}
	switch len(matches) {
	case 0:
		return drive{}, errDriveNotReconnected
	case 1:
		return matches[0], nil
	default:
		paths := make([]string, len(matches))
		for i, m := range matches {
			paths[i] = m.DevicePath
		}
		return drive{}, fmt.Errorf("%w (%s); not writing provisioning data to any of them. Disconnect every drive except the one you just imaged, then retry",
			errDriveAmbiguous, strings.Join(paths, ", "))
	}
}

// lsblkIdentityDevice is one row of
// `lsblk --json --bytes -o NAME,SIZE,TYPE,PTUUID,PARTUUID,LABEL,SERIAL,MODEL <disk>`.
type lsblkIdentityDevice struct {
	Name     string                `json:"name"`
	Size     json.Number           `json:"size"`
	Type     string                `json:"type"`
	PTUUID   string                `json:"ptuuid"`
	PARTUUID string                `json:"partuuid"`
	Label    string                `json:"label"`
	Serial   string                `json:"serial"`
	Model    string                `json:"model"`
	Children []lsblkIdentityDevice `json:"children"`
}

// parseLsblkIdentity reads a driveIdentity from lsblk's JSON for one disk.
func parseLsblkIdentity(out []byte) (driveIdentity, error) {
	var res struct {
		Blockdevices []lsblkIdentityDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return driveIdentity{}, fmt.Errorf("parsing lsblk output: %w", err)
	}
	if len(res.Blockdevices) != 1 || res.Blockdevices[0].Type != "disk" {
		return driveIdentity{}, fmt.Errorf("lsblk did not report exactly one disk")
	}
	disk := res.Blockdevices[0]
	size, err := disk.Size.Int64()
	if err != nil {
		return driveIdentity{}, fmt.Errorf("parsing size of %s: %w", disk.Name, err)
	}
	id := driveIdentity{
		SizeBytes: size,
		DiskGUID:  normalizeGUID(disk.PTUUID),
		Serial:    strings.TrimSpace(disk.Serial),
		Model:     strings.TrimSpace(disk.Model),
	}
	var configGUIDs []string
	for _, p := range disk.Children {
		if strings.EqualFold(strings.TrimSpace(p.Label), "config") {
			configGUIDs = append(configGUIDs, p.PARTUUID)
		}
	}
	// Two "config" partitions: record neither rather than pick one.
	if len(configGUIDs) == 1 {
		id.ConfigPartitionGUID = normalizeGUID(configGUIDs[0])
	}
	return id, nil
}

// psDiskIdentity is one row of the Get-Disk identity query on Windows. Pointer
// fields tolerate the nulls an MBR disk (no Guid) or a bridge without a serial
// reports.
type psDiskIdentity struct {
	Number       int     `json:"Number"`
	Guid         *string `json:"Guid"`
	Size         int64   `json:"Size"`
	SerialNumber *string `json:"SerialNumber"`
	FriendlyName *string `json:"FriendlyName"`
}

// parseWindowsDiskIdentities reads driveIdentity values, keyed by disk number,
// from the Get-Disk identity query's JSON (one object or an array of them).
func parseWindowsDiskIdentities(out []byte) (map[int]driveIdentity, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return map[int]driveIdentity{}, nil
	}
	var rows []psDiskIdentity
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, fmt.Errorf("parsing Get-Disk output: %w", err)
		}
	} else {
		var single psDiskIdentity
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil, fmt.Errorf("parsing Get-Disk output: %w", err)
		}
		rows = []psDiskIdentity{single}
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	ids := make(map[int]driveIdentity, len(rows))
	for _, r := range rows {
		ids[r.Number] = driveIdentity{
			SizeBytes: r.Size,
			DiskGUID:  normalizeGUID(str(r.Guid)),
			Serial:    str(r.SerialNumber),
			Model:     str(r.FriendlyName),
		}
	}
	return ids, nil
}
