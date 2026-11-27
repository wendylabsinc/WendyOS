//go:build darwin || linux || windows

package t234

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// bootloaderLogHead is how bootloader.log starts on an Orin Nano: the QSPI
// erase with its carriage-return progress, then the first partition write.
const bootloaderLogHead = "Erasing /dev/mtd0\n\rErasing 65536 Kibyte @ 0 --  0 % complete \rErasing 65536 Kibyte @ 0 -- 100 % complete \n" +
	"Writing br_bct_BR.bct (size=8192) to BCT (offset=0)\n" +
	"Copied 8192 bytes from br_bct_BR.bct to address 0x00000000 in flash\n"

// ignoredExtraLog is what the initrd's init-extra-pre-wipe writes when the
// image ships no extra script, which is every WendyOS image so far.
const ignoredExtraLog = "No init_extra_pre_wipe was found, ignoring\n"

// The failing lines are what wait_for_storage in the recovery initrd's
// init-flash.sh writes to export-<dev>.log when the device never shows up. The
// other excerpts are from the device logs of installs that worked.
func TestDiagnoseDeviceLogsFindsMissingStorage(t *testing.T) {
	for _, tc := range []struct{ name, file, log, want string }{
		{"NVMe never appeared", "export-nvme0n1.log", "Waiting for /dev/nvme0n1.................[FAIL]\n", "/dev/nvme0n1"},
		{"eMMC never appeared", "export-mmcblk0.log", "Waiting for /dev/mmcblk0.................[FAIL]\n", "/dev/mmcblk0"},
		{"as quoted in the bug report", "export-nvme0n1.log", "Waiting for /dev/nvme0n1 ... FAIL", "/dev/nvme0n1"},
		{"ellipsis character and CRLF", "export-nvme0n1.log", "Waiting for /dev/nvme0n1 … [FAILED]\r\n", "/dev/nvme0n1"},
		{"worded as a timeout", "export-nvme0n1.log", "Waiting for /dev/nvme0n1... timed out\n", "/dev/nvme0n1"},
		{"storage present", "export-nvme0n1.log", "Found /dev/nvme0n1\nLoaded /dev/nvme0n1 on LUN 1\nWaiting for host to eject LUN 1....................[ejected]\n", ""},
		{"storage appeared late", "export-nvme0n1.log", "Waiting for /dev/nvme0n1.....[OK]\nExported /dev/nvme0n1 as nvme0n1\nWaiting for host to connect...[connected]\n", ""},
		{"host never connected", "export-nvme0n1.log", "Found /dev/nvme0n1\nExported /dev/nvme0n1 as nvme0n1\nWaiting for host to connect......", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DiagnoseDeviceLogs(map[string][]byte{"bootloader.log": []byte(bootloaderLogHead), tc.file: []byte(tc.log)})
			if got.MissingStorage != tc.want {
				t.Fatalf("MissingStorage = %q, want %q", got.MissingStorage, tc.want)
			}
		})
	}
}

func TestDiagnoseDeviceLogsBootFirmwareStarted(t *testing.T) {
	for _, tc := range []struct {
		name string
		logs map[string]string
		want bool
	}{
		{"erase and writes logged", map[string]string{"bootloader.log": bootloaderLogHead}, true},
		{"no boot firmware log", map[string]string{"export-nvme0n1.log": "Waiting for /dev/nvme0n1.................[FAIL]\n"}, false},
		{"refused before erasing", map[string]string{"bootloader.log": "ERR: SPI boot device, but /dev/mtd0 device does not exist\n"}, false},
		{"empty boot firmware log", map[string]string{"bootloader.log": ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DiagnoseDeviceLogs(deviceLogs(tc.logs)).BootFirmwareStarted; got != tc.want {
				t.Fatalf("BootFirmwareStarted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiagnoseDeviceLogsEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		logs map[string]string
		want []string
	}{
		{
			name: "the failing line of each log, boot firmware last",
			logs: map[string]string{
				"bootloader.log":            bootloaderLogHead + "ERR: could not retrieve file size of mb1.bin\n",
				"commandloop.log":           "Unrecognized command: frobnicate now\n",
				"custom-extra-pre-wipe.log": ignoredExtraLog,
			},
			want: []string{
				"commandloop.log: Unrecognized command: frobnicate now",
				"bootloader.log: ERR: could not retrieve file size of mb1.bin",
			},
		},
		{
			name: "missing storage",
			logs: map[string]string{
				"bootloader.log":            bootloaderLogHead,
				"custom-extra-pre-wipe.log": ignoredExtraLog,
				"export-nvme0n1.log":        "Waiting for /dev/nvme0n1.................[FAIL]\n",
			},
			want: []string{"export-nvme0n1.log: Waiting for /dev/nvme0n1.................[FAIL]"},
		},
		{
			name: "no failing line: where each log stopped",
			logs: map[string]string{
				"bootloader.log":            bootloaderLogHead,
				"custom-extra-pre-wipe.log": ignoredExtraLog,
				"export-nvme0n1.log":        "Found /dev/nvme0n1\nLoaded /dev/nvme0n1 on LUN 1\nWaiting for host to eject LUN 1.........\n\n",
			},
			want: []string{
				"export-nvme0n1.log: Waiting for host to eject LUN 1.........",
				"bootloader.log: Copied 8192 bytes from br_bct_BR.bct to address 0x00000000 in flash",
			},
		},
		{
			name: "the partition check of a working install is not a failure",
			logs: map[string]string{
				"export-nvme0n1.log": "Found /dev/nvme0n1\n\nCaution: Partition 4 doesn't end on a 2048-sector boundary. This may\n" +
					"result in problems with some disk encryption tools.\n\nNo problems found. 196097133 free sectors (93.5 GiB) available in 8\n" +
					"segments, the largest of which is 196090511 (93.5 GiB) in size.\n  17        52930560        53979135   512.0 MiB   8300  data\n",
			},
			want: []string{"export-nvme0n1.log: 17        52930560        53979135   512.0 MiB   8300  data"},
		},
		{name: "no logs", logs: nil, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DiagnoseDeviceLogs(deviceLogs(tc.logs)).Evidence; !slices.Equal(got, tc.want) {
				t.Fatalf("Evidence = %q\nwant       %q", got, tc.want)
			}
		})
	}
}

// Device log text is printed to a terminal, so it is cut to a few short lines
// and stripped of control characters.
func TestDiagnoseDeviceLogsEvidenceIsBoundedAndPrintable(t *testing.T) {
	many := DiagnoseDeviceLogs(deviceLogs(map[string]string{
		"a.log": "error one\n", "b.log": "error two\n", "c.log": "error three\n", "d.log": "error four\n",
	})).Evidence
	if want := []string{"a.log: error one", "b.log: error two", "c.log: error three"}; !slices.Equal(many, want) {
		t.Fatalf("Evidence = %q, want %q", many, want)
	}
	long := DiagnoseDeviceLogs(deviceLogs(map[string]string{
		"x.log": "write failed: \x1b[31m\a" + strings.Repeat("y", 500) + "\n",
	})).Evidence
	if len(long) != 1 || !strings.HasPrefix(long[0], "x.log: write failed: [31myyy") || strings.ContainsAny(long[0], "\x1b\a") || len([]rune(long[0])) > 200 {
		t.Fatalf("Evidence = %q", long)
	}
}

// The early-failure path end to end, against a status package: the device
// hands it back where the rootfs disk was due, and the logs read out of it
// name the cause.
func TestEarlyFailureCauseIsReadFromTheStatusPackage(t *testing.T) {
	withFastUMSPoll(t)
	image := modeFixture(t, "flashpkg-4k.ext4.gz", "")
	writeFixtureFile(t, image, "flashpkg/status", []byte("FAILED\n"))
	writeFixtureFile(t, image, "flashpkg/logs/export-nvme0n1.log", []byte("Waiting for /dev/nvme0n1.................[FAIL]\n"))
	statusPackage, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	disk := UMSDisk{DevPath: "/dev/fake", RawPath: "/dev/fake", Vendor: FlashpkgVendor, PortPath: "1-3", Serial: "12345678"}
	read := false
	withUMSScan(t, func() ([]UMSDisk, error) {
		if read {
			return nil, nil
		}
		return []UMSDisk{disk}, nil
	})
	stage := &Stage2{
		Plan: &Plan{RootfsDevice: "nvme0n1"}, PortPath: "1-3", Session: "12345678",
		StatusPath: "flashpkg/status", LogsPath: "flashpkg/logs", Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			if req.Writer.DumpTo == "" {
				return nil
			}
			read = true
			return os.WriteFile(req.Writer.DumpTo, statusPackage, 0o644)
		},
	}
	if err := stage.WriteRootfsDevice(context.Background()); !errors.Is(err, ErrDeviceSideFailed) {
		t.Fatalf("WriteRootfsDevice = %v, want ErrDeviceSideFailed", err)
	}
	status, err := stage.AwaitFinalStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Success || status.Status != "FAILED" {
		t.Fatalf("status = %q, success %v", status.Status, status.Success)
	}
	if got := DiagnoseDeviceLogs(status.Logs).MissingStorage; got != "/dev/nvme0n1" {
		t.Fatalf("MissingStorage = %q from logs %v", got, slices.Sorted(maps.Keys(status.Logs)))
	}
}

func deviceLogs(logs map[string]string) map[string][]byte {
	out := map[string][]byte{}
	for name, text := range logs {
		out[name] = []byte(text)
	}
	return out
}
