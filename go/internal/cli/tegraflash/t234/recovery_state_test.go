//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func withUMSScan(t *testing.T, scan func() ([]UMSDisk, error)) {
	t.Helper()
	previous := scanUMSDisks
	scanUMSDisks = scan
	t.Cleanup(func() { scanUMSDisks = previous })
}

func withFastUMSPoll(t *testing.T) {
	t.Helper()
	previous := umsPollInterval
	umsPollInterval = time.Millisecond
	t.Cleanup(func() { umsPollInterval = previous })
}

func TestWaitForUMSDiskAtCorrelatesPortAndSession(t *testing.T) {
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{
			{DevPath: "/dev/wrong", Vendor: "nvme0n1", PortPath: "1-2", Serial: "aaaaaaaa"},
			{DevPath: "/dev/right", Vendor: "nvme0n1", PortPath: "1-3", Serial: "bbbbbbbb"},
		}, nil
	})
	disk, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: "nvme0n1", PortPath: "1-3", Session: "BBBBBBBB"}, time.Second)
	if err != nil || disk.DevPath != "/dev/right" {
		t.Fatalf("correlated disk = %+v, err=%v", disk, err)
	}
}

func TestWaitForUMSDiskAtRejectsMissingTopology(t *testing.T) {
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{{DevPath: "/dev/sdz", Vendor: FlashpkgVendor, Serial: "12345678"}}, nil
	})
	_, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-3"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "physical USB port could not be determined") {
		t.Fatalf("missing-topology error = %v", err)
	}
}

func TestWaitForUMSDiskAtRejectsAmbiguity(t *testing.T) {
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{
			{DevPath: "/dev/a", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"},
			{DevPath: "/dev/b", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"},
		}, nil
	})
	_, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-3", Session: "12345678"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity error = %v", err)
	}
}

func TestValidateDeviceIdentity(t *testing.T) {
	want := IdentityExpectation{ModuleID: "3701", ModuleSKU: "0005", CarrierID: "3737", CarrierSKU: "0000"}
	got := DeviceIdentity{Protocol: DeviceIdentityProtocol, SessionID: "ABCDEF12", ModuleID: "3701", ModuleSKU: "0005", CarrierID: "3737", CarrierSKU: "0000"}
	if err := validateDeviceIdentity(got, "abcdef12", want); err != nil {
		t.Fatal(err)
	}
	got.ModuleSKU = "0004"
	if err := validateDeviceIdentity(got, "abcdef12", want); err == nil || !strings.Contains(err.Error(), "wrong Jetson hardware") {
		t.Fatalf("wrong-SKU error = %v", err)
	}
	got.ModuleSKU = "0005"
	got.SessionID = "00000000"
	if err := validateDeviceIdentity(got, "abcdef12", want); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong-session error = %v", err)
	}
	got.SessionID = "abcdef12"
	got.Protocol = "legacy"
	if err := validateDeviceIdentity(got, "abcdef12", want); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("wrong-protocol error = %v", err)
	}
	// An initrd that failed to parse its device tree reports UNKNOWN (seen live
	// on an Orin Nano whose DTB compatible carries a "-super" suffix). That is
	// "identity unreadable", not "wrong hardware" — the error must say the
	// flashpack's initrd is at fault, and still refuse the flash.
	got.Protocol = DeviceIdentityProtocol
	got.ModuleID, got.ModuleSKU, got.CarrierID, got.CarrierSKU = "UNKNOWN", "", "UNKNOWN", ""
	err := validateDeviceIdentity(got, "abcdef12", want)
	if err == nil || !strings.Contains(err.Error(), "could not identify") || strings.Contains(err.Error(), "wrong Jetson hardware") {
		t.Fatalf("unknown-identity error = %v", err)
	}
}

func TestRootfsWaitToleratesStaleFlashpkgTransition(t *testing.T) {
	withFastUMSPoll(t)
	calls := 0
	withUMSScan(t, func() ([]UMSDisk, error) {
		calls++
		if calls <= 2 {
			return []UMSDisk{{DevPath: "/dev/flashpkg", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}}, nil
		}
		return []UMSDisk{{DevPath: "/dev/rootfs", Vendor: "nvme0n1", PortPath: "1-3", Serial: "12345678"}}, nil
	})
	disk, err := waitForUMSDiskConfirmed(context.Background(), LUNSelector{Vendor: "nvme0n1", PortPath: "1-3", Session: "12345678"}, time.Second)
	if err != nil || disk.DevPath != "/dev/rootfs" {
		t.Fatalf("transition result = %+v, %v", disk, err)
	}
}

func TestRootfsWaitDetectsPersistentEarlyFinalStatus(t *testing.T) {
	withFastUMSPoll(t)
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{{DevPath: "/dev/flashpkg", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}}, nil
	})
	_, err := waitForUMSDiskConfirmed(context.Background(), LUNSelector{Vendor: "nvme0n1", PortPath: "1-3", Session: "12345678"}, time.Second)
	if err != errGotFlashpkg {
		t.Fatalf("early-status error = %v", err)
	}
}

func TestUMSWaitCancellationAndTimeout(t *testing.T) {
	withFastUMSPoll(t)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := WaitForUMSDiskAt(ctx, LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-3"}, time.Second); err != context.Canceled {
		t.Fatalf("cancel error = %v", err)
	}
	if _, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-3"}, 3*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestWaitForDiskGoneTracksExactLUN(t *testing.T) {
	withFastUMSPoll(t)
	calls := 0
	disk := UMSDisk{DevPath: "/dev/rootfs", Vendor: "nvme0n1", PortPath: "1-3", Serial: "12345678"}
	withUMSScan(t, func() ([]UMSDisk, error) {
		calls++
		if calls < 3 {
			return []UMSDisk{disk}, nil
		}
		return nil, nil
	})
	gone, err := (&Stage2{}).waitForDiskGone(context.Background(), disk)
	if err != nil || !gone {
		t.Fatalf("waitForDiskGone = %v, %v", gone, err)
	}
}

func TestAwaitFinalStatusCollectsLogsAndRequiresSuccess(t *testing.T) {
	withFastUMSPoll(t)
	disk := UMSDisk{DevPath: "/dev/fake", RawPath: "/dev/fake", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	calls := 0
	withUMSScan(t, func() ([]UMSDisk, error) {
		calls++
		if calls == 1 {
			return []UMSDisk{disk}, nil
		}
		return nil, nil
	})
	fixture, err := io.ReadAll(openFixture(t, "flashpkg-4k.ext4.gz"))
	if err != nil {
		t.Fatal(err)
	}
	stage := &Stage2{
		PortPath: "1-3", Session: "12345678", StatusPath: "flashpkg/status", LogsPath: "flashpkg/logs", Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Writer.DumpTo != "" {
				return os.WriteFile(req.Writer.DumpTo, fixture, 0o644)
			}
			return nil
		},
	}
	status, err := stage.AwaitFinalStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Success || status.Status != "SUCCESS" || len(status.Logs["big.log"]) != 300000 {
		t.Fatalf("final status = %+v", status)
	}
}

// After RCM boot the gadget can train at a different USB speed than the
// bootROM's recovery device, and USB2/USB3 phys of one physical connector are
// distinct root-hub ports (seen live on macOS: recovery at usb 1-1, SuperSpeed
// gadget at usb 1-2). The first-LUN wait must treat the recovery port as a
// hint: exact match preferred, a unique off-port candidate accepted, several
// candidates fail closed.
func TestFirstLUNWaitAcceptsUniqueOffPortGadget(t *testing.T) {
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{{DevPath: "/dev/disk6", Vendor: FlashpkgVendor, PortPath: "1-2", Serial: "f3885343"}}, nil
	})
	disk, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-1", PortHint: true}, time.Second)
	if err != nil || disk.DevPath != "/dev/disk6" {
		t.Fatalf("off-port gadget = %+v, err=%v", disk, err)
	}
}

func TestFirstLUNWaitPrefersExactPortMatch(t *testing.T) {
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{
			{DevPath: "/dev/other", Vendor: FlashpkgVendor, PortPath: "3-2", Serial: "aaaaaaaa"},
			{DevPath: "/dev/mine", Vendor: FlashpkgVendor, PortPath: "1-1", Serial: "bbbbbbbb"},
		}, nil
	})
	disk, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-1", PortHint: true}, time.Second)
	if err != nil || disk.DevPath != "/dev/mine" {
		t.Fatalf("exact-port disk = %+v, err=%v", disk, err)
	}
}

func TestFirstLUNWaitRejectsMultipleOffPortCandidates(t *testing.T) {
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{
			{DevPath: "/dev/a", Vendor: FlashpkgVendor, PortPath: "1-2", Serial: "aaaaaaaa"},
			{DevPath: "/dev/b", Vendor: FlashpkgVendor, PortPath: "3-2", Serial: "bbbbbbbb"},
		}, nil
	})
	_, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: FlashpkgVendor, PortPath: "1-1", PortHint: true}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "none is at the expected port") {
		t.Fatalf("multi-candidate error = %v", err)
	}
}

// Without PortHint a wait pins the gadget's own port + session; an off-port LUN
// must never satisfy it.
func TestSubsequentLUNWaitRequiresExactPort(t *testing.T) {
	withFastUMSPoll(t)
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{{DevPath: "/dev/other", Vendor: "nvme0n1", PortPath: "1-2", Serial: "f3885343"}}, nil
	})
	_, err := WaitForUMSDiskAt(context.Background(), LUNSelector{Vendor: "nvme0n1", PortPath: "1-1", Session: "f3885343"}, 3*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("off-port subsequent wait error = %v", err)
	}
}

func TestAdoptGadgetRepinsPortAndSession(t *testing.T) {
	s := &Stage2{PortPath: "1-1", Out: io.Discard}
	s.adoptGadget(UMSDisk{DevPath: "/dev/disk6", Vendor: FlashpkgVendor, PortPath: "1-2", Serial: "f3885343"})
	if s.PortPath != "1-2" || s.Session != "f3885343" {
		t.Fatalf("adopted state: port=%q session=%q", s.PortPath, s.Session)
	}
}

func withFastReattachWait(t *testing.T) {
	t.Helper()
	previous := identityReattachWait
	identityReattachWait = 5 * time.Millisecond
	t.Cleanup(func() { identityReattachWait = previous })
}

func withFastIdentityRetry(t *testing.T) {
	t.Helper()
	previous := identityRetryDelay
	identityRetryDelay = time.Millisecond
	t.Cleanup(func() { identityRetryDelay = previous })
}

// identityExpectation matches the flashpkg-identity-1k fixture's device.json.
var identityExpectation = IdentityExpectation{ModuleID: "3767", ModuleSKU: "0005", CarrierID: "3768", CarrierSKU: "0000"}

// A macOS DiskArbitration eject (e.g. the "disk not readable" dialog's default
// Eject button) can detach the flashpkg LUN between enumeration and the
// identity read; the LUN then re-attaches under a new disk number (seen live
// as ENXIO on an Orin Nano, WDY-2621). The identity read must re-resolve the
// LUN by port/session and retry instead of aborting the flash.
func TestVerifyDeviceIdentityRetriesAfterLUNDetach(t *testing.T) {
	withFastUMSPoll(t)
	withFastIdentityRetry(t)
	fixture, err := io.ReadAll(openFixture(t, "flashpkg-identity-1k.ext4.gz"))
	if err != nil {
		t.Fatal(err)
	}
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{{DevPath: "/dev/disk9", RawPath: "/dev/rdisk9", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}}, nil
	})
	dumps := 0
	stage := &Stage2{
		PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Writer.DumpTo == "" {
				return nil
			}
			dumps++
			if dumps == 1 {
				return errors.New("reading /dev/rdisk8: read /dev/rdisk8: device not configured")
			}
			if req.Writer.Device != "/dev/rdisk9" {
				t.Errorf("retry read %s, want the re-resolved /dev/rdisk9", req.Writer.Device)
			}
			return os.WriteFile(req.Writer.DumpTo, fixture, 0o644)
		},
	}
	disk := UMSDisk{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	got, err := stage.verifyDeviceIdentity(context.Background(), disk)
	if err != nil {
		t.Fatalf("verifyDeviceIdentity = %v", err)
	}
	if got.RawPath != "/dev/rdisk9" || dumps != 2 {
		t.Fatalf("re-resolved disk = %+v after %d dumps", got, dumps)
	}
}

func TestVerifyDeviceIdentityReportsDetachWhenLUNGone(t *testing.T) {
	withFastUMSPoll(t)
	withFastReattachWait(t)
	withFastIdentityRetry(t)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	stage := &Stage2{
		PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			return errors.New("reading /dev/rdisk8: read /dev/rdisk8: device not configured")
		},
	}
	disk := UMSDisk{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	_, err := stage.verifyDeviceIdentity(context.Background(), disk)
	if err == nil || !strings.Contains(err.Error(), "device not configured") || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("detach error = %v", err)
	}
	// The re-scan's own verdict must survive: it distinguishes "never came
	// back" (timed out) from a fail-closed correlation refusal.
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("detach error hides the re-scan failure: %v", err)
	}
}

// SendFlashPackage size-checks the LUN it enumerated; a re-attached LUN must
// pass the same check rather than slip through to a short read.
func TestVerifyDeviceIdentityRejectsUndersizedReattachedLUN(t *testing.T) {
	withFastUMSPoll(t)
	withFastIdentityRetry(t)
	withUMSScan(t, func() ([]UMSDisk, error) {
		return []UMSDisk{{DevPath: "/dev/disk9", RawPath: "/dev/rdisk9", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678", SizeBytes: flashpkgSize / 2}}, nil
	})
	stage := &Stage2{
		PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			return errors.New("reading /dev/rdisk8: read /dev/rdisk8: device not configured")
		},
	}
	disk := UMSDisk{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678", SizeBytes: flashpkgSize}
	_, err := stage.verifyDeviceIdentity(context.Background(), disk)
	if err == nil || !strings.Contains(err.Error(), "smaller") {
		t.Fatalf("undersized re-attach error = %v", err)
	}
}

// The identity read is the last non-destructive step, and unmounting first
// gains nothing (the LUN carries no host-mountable filesystem): read the
// identity before touching the disk, and only unmount ahead of the write.
func TestSendFlashPackageVerifiesIdentityBeforeUnmount(t *testing.T) {
	withFastUMSPoll(t)
	withPollingMissing(t, true)
	fixture, err := io.ReadAll(openFixture(t, "flashpkg-identity-1k.ext4.gz"))
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "flashpkg.ext4")
	if err := os.WriteFile(pkg, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	var ops []string
	ejected := false
	withUMSScan(t, func() ([]UMSDisk, error) {
		if ejected {
			return nil, nil
		}
		return []UMSDisk{{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678", SizeBytes: flashpkgSize}}, nil
	})
	stage := &Stage2{
		FlashPackagePath: pkg, PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			switch {
			case req.PollMedia:
				ops = append(ops, "poll "+req.Session)
			case req.Unmount:
				ops = append(ops, "unmount")
			case req.Eject:
				ops = append(ops, "eject")
				ejected = true
			case req.Writer.DumpTo != "":
				ops = append(ops, "dump")
				return os.WriteFile(req.Writer.DumpTo, fixture, 0o644)
			case req.Writer.Blob != "":
				ops = append(ops, "write")
			}
			return nil
		},
	}
	if err := stage.SendFlashPackage(context.Background()); err != nil {
		t.Fatalf("SendFlashPackage = %v", err)
	}
	// Polling is enabled before the first eject, so later media changes are seen.
	if want := []string{"dump", "poll 12345678", "unmount", "write", "dump", "eject"}; !slices.Equal(ops, want) {
		t.Fatalf("helper ops = %v, want %v", ops, want)
	}
}

// Exhausting the read attempts while the LUN stays visible must surface the
// read error itself, after exactly identityReadAttempts dumps.
func TestVerifyDeviceIdentityGivesUpAfterBoundedAttempts(t *testing.T) {
	withFastUMSPoll(t)
	withFastIdentityRetry(t)
	disk := UMSDisk{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	withUMSScan(t, func() ([]UMSDisk, error) { return []UMSDisk{disk}, nil })
	dumps := 0
	stage := &Stage2{
		PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			dumps++
			return errors.New("input/output error")
		},
	}
	_, err := stage.verifyDeviceIdentity(context.Background(), disk)
	if err == nil || !strings.Contains(err.Error(), "input/output error") || strings.Contains(err.Error(), "detached") {
		t.Fatalf("exhaustion error = %v", err)
	}
	if dumps != 3 {
		t.Fatalf("dump attempts = %d, want 3", dumps)
	}
}

// A same-node transient (raw reads failing while the LUN stays listed) can
// clear within a second; back-to-back retries would exhaust every attempt
// inside the blip. Each retry must wait identityRetryDelay before re-scanning.
func TestVerifyDeviceIdentityPacesRetries(t *testing.T) {
	withFastUMSPoll(t)
	previous := identityRetryDelay
	identityRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { identityRetryDelay = previous })
	fixture, err := io.ReadAll(openFixture(t, "flashpkg-identity-1k.ext4.gz"))
	if err != nil {
		t.Fatal(err)
	}
	disk := UMSDisk{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678", SizeBytes: flashpkgSize}
	withUMSScan(t, func() ([]UMSDisk, error) { return []UMSDisk{disk}, nil })
	dumps := 0
	stage := &Stage2{
		PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			dumps++
			if dumps == 1 {
				return errors.New("reading /dev/rdisk8: read /dev/rdisk8: device not configured")
			}
			return os.WriteFile(req.Writer.DumpTo, fixture, 0o644)
		},
	}
	start := time.Now()
	if _, err := stage.verifyDeviceIdentity(context.Background(), disk); err != nil {
		t.Fatalf("verifyDeviceIdentity = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("retry after %v, want at least the %v pacing delay", elapsed, 50*time.Millisecond)
	}
}

func TestVerifyDeviceIdentityReturnsCancellationNotDetach(t *testing.T) {
	withFastUMSPoll(t)
	withUMSScan(t, func() ([]UMSDisk, error) { return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	stage := &Stage2{
		PortPath: "1-3", ExpectedIdentity: identityExpectation, Out: io.Discard, TempDir: t.TempDir(),
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			cancel()
			return errors.New("read interrupted")
		},
	}
	disk := UMSDisk{DevPath: "/dev/disk8", RawPath: "/dev/rdisk8", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	if _, err := stage.verifyDeviceIdentity(ctx, disk); err != context.Canceled {
		t.Fatalf("cancellation error = %v", err)
	}
}
