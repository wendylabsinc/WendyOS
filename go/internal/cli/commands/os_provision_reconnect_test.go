//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// imagedID is the identity recorded for the drive under test.
var imagedID = driveIdentity{
	SizeBytes:           1_000_204_886_016,
	ConfigPartitionGUID: "5c9f2b8e-1111-4222-8333-944455556666",
	Model:               "RTL9210C NVME",
}

// enxioWriteErr is the WDY-3974 failure: the enclosure dropped off the bus
// while the agent binary was being copied onto the mounted config partition.
var enxioWriteErr = fmt.Errorf("writing wendy-agent to config partition: %w",
	&os.PathError{Op: "write", Path: "/Volumes/config/wendy-agent", Err: syscall.ENXIO})

func TestDriveIdentityMatches(t *testing.T) {
	other := func(edit func(*driveIdentity)) driveIdentity {
		id := imagedID
		edit(&id)
		return id
	}
	tests := []struct {
		name  string
		ref   driveIdentity
		cand  driveIdentity
		match bool
	}{
		{name: "identical", ref: imagedID, cand: imagedID, match: true},
		{name: "GUID case differs", ref: imagedID, cand: other(func(id *driveIdentity) { id.ConfigPartitionGUID = strings.ToUpper(id.ConfigPartitionGUID) }), match: true},
		{name: "different size", ref: imagedID, cand: other(func(id *driveIdentity) { id.SizeBytes-- })},
		{name: "different config partition", ref: imagedID, cand: other(func(id *driveIdentity) { id.ConfigPartitionGUID = "0000" })},
		{name: "config partition not enumerated yet", ref: imagedID, cand: other(func(id *driveIdentity) { id.ConfigPartitionGUID = "" })},
		{name: "different model", ref: imagedID, cand: other(func(id *driveIdentity) { id.Model = "Samsung PSSD T7" })},
		{name: "candidate has fields the reference lacks", ref: imagedID, cand: other(func(id *driveIdentity) { id.DiskGUID, id.Serial = "abcd", "1234" }), match: true},
		{name: "reference without a GUID never matches", ref: driveIdentity{SizeBytes: imagedID.SizeBytes, Model: imagedID.Model}, cand: driveIdentity{SizeBytes: imagedID.SizeBytes, Model: imagedID.Model}},
		{name: "reference without a size never matches", ref: driveIdentity{DiskGUID: "abcd"}, cand: driveIdentity{DiskGUID: "abcd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.matches(tt.cand); got != tt.match {
				t.Errorf("matches() = %v, want %v", got, tt.match)
			}
		})
	}
}

func TestDriveDisconnected(t *testing.T) {
	orig := drivePresentFn
	t.Cleanup(func() { drivePresentFn = orig })

	tests := []struct {
		name    string
		err     error
		present bool
		want    bool
	}{
		{name: "ENXIO from the write", err: enxioWriteErr, present: true, want: true},
		{name: "ENXIO text from the elevated cp", err: errors.New("writing wendy-agent to config partition: cp: /Volumes/config/wendy-agent: Device not configured: exit status 1"), present: true, want: true},
		{name: "Linux ENXIO text", err: errors.New("write /tmp/wendyos-config-1/wendy.conf: no such device or address"), present: true, want: true},
		{name: "disk node gone", err: errors.New("locating config partition on /dev/disk8: diskutil list /dev/disk8: exit status 1"), present: false, want: true},
		{name: "reconnect wait came up empty", err: fmt.Errorf("%w within 2m0s", errDriveNotReconnected), present: false, want: true},
		{name: "drive present, ordinary failure", err: errors.New("mounting config partition /dev/disk4s2: resource busy"), present: true, want: false},
		{name: "drive present, download failure", err: errors.New("resolving agent binary: network unreachable"), present: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drivePresentFn = func(drive) bool { return tt.present }
			if got := driveDisconnected(drive{DevicePath: "/dev/disk8"}, tt.err); got != tt.want {
				t.Errorf("driveDisconnected() = %v, want %v", got, tt.want)
			}
		})
	}
}

// WDY-3974: after the drive drops off the bus, a retry must wait for it to come
// back and write to wherever it reappears — and only there.
func TestProvisionConfigWithRetry_DisconnectedDriveIsFoundAgainByIdentity(t *testing.T) {
	original := drive{DevicePath: "/dev/disk8", Name: "RTL9210C NVME"}
	var written []string
	calls := withStubbedProvisioning(t, func(d drive, _ []wendyconf.WifiCredential, _ string, _ []byte) error {
		written = append(written, d.DevicePath)
		if d.DevicePath == original.DevicePath {
			return enxioWriteErr
		}
		return nil
	}, true, []bool{true})
	readDriveIdentityFn = func(d drive) (driveIdentity, error) {
		if d.DevicePath != original.DevicePath {
			t.Errorf("identity read from %s; want only the imaged drive %s", d.DevicePath, original.DevicePath)
		}
		return imagedID, nil
	}
	drivePresentFn = func(drive) bool { return false }
	reconnectTimeout = time.Hour

	// Listing fails while the bus re-enumerates, then shows nothing, then the
	// drive reappears as disk9 — next to an unrelated disk that took disk8.
	polls := 0
	listIdentifiedDrivesFn = func() ([]identifiedDrive, error) {
		polls++
		switch polls {
		case 1:
			return nil, errors.New("running diskutil: exit status 1")
		case 2:
			return nil, nil
		default:
			stranger := imagedID
			stranger.SizeBytes = 32_000_000_000
			return []identifiedDrive{
				{drive: drive{DevicePath: "/dev/disk8"}, id: stranger},
				{drive: drive{DevicePath: "/dev/disk9", Name: "RTL9210C NVME"}, id: imagedID},
			}, nil
		}
	}

	var got drive
	var attached bool
	var err error
	out, _ := captureCommandStdout(t, func() error {
		got, attached, err = provisionConfigWithRetry(context.Background(), original, nil, "brave-dolphin", nil, true)
		return nil
	})

	if err != nil {
		t.Fatalf("provisionConfigWithRetry() error = %v; want success after the drive reconnected", err)
	}
	if *calls != 2 || written[1] != "/dev/disk9" {
		t.Errorf("writes went to %v; want the failed /dev/disk8 attempt, then /dev/disk9", written)
	}
	if got.DevicePath != "/dev/disk9" || !attached {
		t.Errorf("returned drive %q attached=%v; want /dev/disk9 attached", got.DevicePath, attached)
	}
	for _, want := range []string{"disconnected", "Unplug the drive and plug it back in", "Found the drive again at /dev/disk9"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A drive that reappears at the old path but is not the one just imaged must
// never be written; the retry waits, gives up, and asks again.
func TestProvisionConfigWithRetry_DisconnectedDriveNotFoundIsNeverWritten(t *testing.T) {
	calls := withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return enxioWriteErr
	}, true, []bool{true, false})
	readDriveIdentityFn = func(drive) (driveIdentity, error) { return imagedID, nil }
	drivePresentFn = func(drive) bool { return false }
	listIdentifiedDrivesFn = func() ([]identifiedDrive, error) {
		different := imagedID
		different.ConfigPartitionGUID = "0f0f0f0f-0000-4000-8000-000000000000"
		return []identifiedDrive{{drive: drive{DevicePath: "/dev/disk8"}, id: different}}, nil
	}

	var attached bool
	var err error
	out, _ := captureCommandStdout(t, func() error {
		_, attached, err = provisionConfigWithRetry(context.Background(), drive{DevicePath: "/dev/disk8"}, nil, "brave-dolphin", nil, true)
		return nil
	})

	if *calls != 1 {
		t.Errorf("provision called %d times; want 1 (no write to an unconfirmed drive)", *calls)
	}
	if !errors.Is(err, errDriveNotReconnected) {
		t.Errorf("error = %v; want it to report that the drive did not reconnect", err)
	}
	if attached {
		t.Error("attached = true; the old device path is not the imaged drive and must not be ejected")
	}
	if strings.Count(out, "Unplug the drive") != 2 {
		t.Errorf("want the replug guidance after the failure and again after the wait; got:\n%s", out)
	}
}

// Two attached drives that both look like the one just written: refuse rather
// than pick one.
func TestProvisionConfigWithRetry_AmbiguousMatchIsNeverWritten(t *testing.T) {
	calls := withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return enxioWriteErr
	}, true, []bool{true, false})
	readDriveIdentityFn = func(drive) (driveIdentity, error) { return imagedID, nil }
	drivePresentFn = func(drive) bool { return false }
	listIdentifiedDrivesFn = func() ([]identifiedDrive, error) {
		return []identifiedDrive{
			{drive: drive{DevicePath: "/dev/disk9"}, id: imagedID},
			{drive: drive{DevicePath: "/dev/disk10"}, id: imagedID},
		}, nil
	}

	var err error
	out, _ := captureCommandStdout(t, func() error {
		_, _, err = provisionConfigWithRetry(context.Background(), drive{DevicePath: "/dev/disk8"}, nil, "brave-dolphin", nil, true)
		return nil
	})

	if *calls != 1 {
		t.Errorf("provision called %d times; want 1 (no write when the match is ambiguous)", *calls)
	}
	if !errors.Is(err, errDriveAmbiguous) {
		t.Errorf("error = %v; want the ambiguous-match refusal", err)
	}
	if !strings.Contains(out, "/dev/disk9, /dev/disk10") {
		t.Errorf("output should name the matching drives; got:\n%s", out)
	}
}

// Ctrl-C while waiting for the drive ends the wait like declining the retry,
// instead of leaving the user stuck until the timeout.
func TestProvisionConfigWithRetry_CancelWhileWaitingStops(t *testing.T) {
	calls := withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return enxioWriteErr
	}, true, []bool{true, true})
	readDriveIdentityFn = func(drive) (driveIdentity, error) { return imagedID, nil }
	drivePresentFn = func(drive) bool { return false }
	reconnectTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconnectSleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	var attached bool
	var err error
	out, _ := captureCommandStdout(t, func() error {
		_, attached, err = provisionConfigWithRetry(ctx, drive{DevicePath: "/dev/disk8"}, nil, "brave-dolphin", nil, true)
		return nil
	})

	if *calls != 1 || !errors.Is(err, syscall.ENXIO) || attached {
		t.Errorf("calls=%d err=%v attached=%v; want one attempt, the write error, attached=false", *calls, err, attached)
	}
	if !strings.Contains(out, "Skipping provisioning") {
		t.Errorf("want the decline guidance after Ctrl-C; got:\n%s", out)
	}
}

// Without an identity recorded before the disconnect, a reconnected drive can
// never be confirmed, so no retry is offered at all.
func TestProvisionConfigWithRetry_DisconnectWithoutIdentityStops(t *testing.T) {
	calls := withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return enxioWriteErr
	}, true, nil)
	readDriveIdentityFn = func(drive) (driveIdentity, error) {
		return driveIdentity{}, errors.New("config partition not found on /dev/disk8")
	}
	prompted := false
	confirmProvisioningRetry = func() (bool, error) {
		prompted = true
		return true, nil
	}
	listIdentifiedDrivesFn = func() ([]identifiedDrive, error) {
		t.Error("listed drives although the imaged drive's identity is unknown")
		return nil, nil
	}

	var attached bool
	var err error
	out, _ := captureCommandStdout(t, func() error {
		_, attached, err = provisionConfigWithRetry(context.Background(), drive{DevicePath: "/dev/disk8"}, nil, "brave-dolphin", nil, true)
		return nil
	})

	if prompted || *calls != 1 {
		t.Errorf("prompted=%v, provision calls=%d; want no retry offered", prompted, *calls)
	}
	if !errors.Is(err, syscall.ENXIO) || attached {
		t.Errorf("err=%v attached=%v; want the original write error and attached=false", err, attached)
	}
	if !strings.Contains(out, "cannot confirm") {
		t.Errorf("output should explain why it will not retry; got:\n%s", out)
	}
}

// Non-interactive runs keep their behaviour: one attempt, the warning, and the
// error when provisioning was requested. No identity is read.
func TestProvisionConfigWithRetry_NonInteractiveDisconnect(t *testing.T) {
	calls := withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return enxioWriteErr
	}, false, nil)
	readDriveIdentityFn = func(drive) (driveIdentity, error) {
		t.Error("read the drive identity on a non-interactive run")
		return imagedID, nil
	}

	var attached bool
	var err error
	out, _ := captureCommandStdout(t, func() error {
		_, attached, err = provisionConfigWithRetry(context.Background(), drive{DevicePath: "/dev/disk8"}, nil, "brave-dolphin", nil, true)
		return nil
	})

	if *calls != 1 || !errors.Is(err, syscall.ENXIO) {
		t.Errorf("calls=%d err=%v; want one attempt returning the write error", *calls, err)
	}
	if attached {
		t.Error("attached = true; a drive that disconnected must not be ejected by its old path")
	}
	if !strings.Contains(out, "Re-run 'wendy os install'") || strings.Contains(out, "Unplug") {
		t.Errorf("unexpected non-interactive output:\n%s", out)
	}
}

func TestParseLsblkIdentity(t *testing.T) {
	out := []byte(`{"blockdevices": [
	  {"name":"sdb", "size":1000204886016, "type":"disk", "ptuuid":"A1B2C3D4-0000-4000-8000-000000000001", "partuuid":null, "label":null, "serial":"012345678904 ", "model":"RTL9210C NVME   ",
	   "children": [
	      {"name":"sdb1", "size":67108864, "type":"part", "ptuuid":"a1b2c3d4-0000-4000-8000-000000000001", "partuuid":"11111111-0000-4000-8000-000000000001", "label":"APP", "serial":null, "model":null},
	      {"name":"sdb2", "size":67108864, "type":"part", "ptuuid":"a1b2c3d4-0000-4000-8000-000000000001", "partuuid":"22222222-0000-4000-8000-000000000002", "label":"config", "serial":null, "model":null}
	   ]}
	]}`)
	got, err := parseLsblkIdentity(out)
	if err != nil {
		t.Fatalf("parseLsblkIdentity: %v", err)
	}
	want := driveIdentity{
		SizeBytes:           1000204886016,
		DiskGUID:            "a1b2c3d4-0000-4000-8000-000000000001",
		ConfigPartitionGUID: "22222222-0000-4000-8000-000000000002",
		Serial:              "012345678904",
		Model:               "RTL9210C NVME",
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}

	// Older lsblk prints sizes as strings; two "config" partitions record neither.
	got, err = parseLsblkIdentity([]byte(`{"blockdevices":[{"name":"sdc","size":"31914983424","type":"disk","ptuuid":"abcd",
	  "children":[{"name":"sdc1","type":"part","partuuid":"p1","label":"config"},{"name":"sdc2","type":"part","partuuid":"p2","label":"CONFIG"}]}]}`))
	if err != nil {
		t.Fatalf("parseLsblkIdentity (string size): %v", err)
	}
	if got.SizeBytes != 31914983424 || got.DiskGUID != "abcd" || got.ConfigPartitionGUID != "" {
		t.Errorf("got %+v; want size 31914983424, disk GUID abcd, no config-partition GUID", got)
	}

	if _, err := parseLsblkIdentity([]byte(`{"blockdevices":[]}`)); err == nil {
		t.Error("expected an error when lsblk reports no disk")
	}
}

func TestParseWindowsDiskIdentities(t *testing.T) {
	single := []byte(`{"Number":3,"Guid":"{A1B2C3D4-0000-4000-8000-000000000001}","Size":1000204886016,"SerialNumber":"012345678904  ","FriendlyName":"Realtek RTL9210C NVME"}`)
	got, err := parseWindowsDiskIdentities(single)
	if err != nil {
		t.Fatalf("single object: %v", err)
	}
	want := driveIdentity{
		SizeBytes: 1000204886016,
		DiskGUID:  "a1b2c3d4-0000-4000-8000-000000000001",
		Serial:    "012345678904",
		Model:     "Realtek RTL9210C NVME",
	}
	if got[3] != want {
		t.Errorf("disk 3 = %+v\nwant %+v", got[3], want)
	}

	array := []byte(`[{"Number":0,"Guid":null,"Size":256060514304,"SerialNumber":null,"FriendlyName":"Internal MBR"},` +
		`{"Number":3,"Guid":"{A1B2C3D4-0000-4000-8000-000000000001}","Size":1000204886016,"SerialNumber":"012345678904","FriendlyName":"Realtek RTL9210C NVME"}]`)
	got, err = parseWindowsDiskIdentities(array)
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	if len(got) != 2 || got[0].DiskGUID != "" || got[0].usable() || got[3] != want {
		t.Errorf("got %+v; want an unusable MBR disk 0 and disk 3 = %+v", got, want)
	}
}
