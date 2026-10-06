//go:build darwin || linux || windows

package t234

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

func modeFixture(t *testing.T, fixture, marker string) string {
	t.Helper()
	data, err := io.ReadAll(openFixture(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "commands.ext4")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if marker != "" {
		writeFixtureFile(t, path, usbModePath, []byte(marker))
	}
	return path
}

func writeFixtureFile(t *testing.T, path, name string, data []byte) {
	t.Helper()
	image, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	info, err := image.Stat()
	if err != nil {
		t.Fatal(err)
	}
	fs, err := ext4.Read(backendfile.New(image, false), info.Size(), 0, 512)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	file, err := fs.OpenFile(name, os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := image.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareUSBModeCompatibilityMatrix(t *testing.T) {
	for _, fixture := range []string{"flashpkg-1k.ext4.gz", "flashpkg-4k.ext4.gz"} {
		for _, marker := range []string{"", "legacy", "legacy\n", "single", "single\n", "unknown\n", "legacy\nextra"} {
			for _, capable := range []bool{false, true} {
				name := fixture + "/" + strings.TrimSpace(marker)
				if capable {
					name += "/capable"
				}
				t.Run(name, func(t *testing.T) {
					source := modeFixture(t, fixture, marker)
					before, err := os.ReadFile(source)
					if err != nil {
						t.Fatal(err)
					}
					temp := t.TempDir()
					prepared, mode, err := prepareUSBMode(source, temp, capable)
					invalid := marker == "unknown\n" || marker == "legacy\nextra" || strings.HasPrefix(marker, "single") && !capable
					if invalid {
						if err == nil {
							t.Fatal("unsupported mode was accepted")
						}
						files, _ := os.ReadDir(temp)
						if len(files) != 0 {
							t.Fatal("failed preparation left temporary files")
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						wantMode := USBModeLegacy
						if capable {
							wantMode = USBModeSingle
						}
						if mode != wantMode {
							t.Fatalf("mode = %q, want %q", mode, wantMode)
						}
						wantCopy := capable && !strings.HasPrefix(marker, "single")
						if (prepared != source) != wantCopy {
							t.Fatalf("prepared path = %s, expected private copy: %v", prepared, wantCopy)
						}
						image, err := os.Open(prepared)
						if err != nil {
							t.Fatal(err)
						}
						defer image.Close()
						got, err := readPackageUSBMode(image)
						if err != nil || got != wantMode {
							t.Fatalf("readback = %q, %v", got, err)
						}
						for _, name := range []string{"flashpkg/status", "flashpkg/conf/command_sequence", "flashpkg/logs/big.log"} {
							want, err := Ext4ReadFile(bytes.NewReader(before), name)
							if err != nil {
								t.Fatal(err)
							}
							got, err := Ext4ReadFile(image, name)
							if err != nil || !bytes.Equal(got, want) {
								t.Fatalf("%s changed: %v", name, err)
							}
						}
						// A future package already selecting single bypasses the editor.
						if capable {
							again, gotMode, err := prepareUSBMode(prepared, temp, true)
							if err != nil || again != prepared || gotMode != USBModeSingle {
								t.Fatalf("already-selected package: %s, %q, %v", again, gotMode, err)
							}
						}
					}
					after, err := os.ReadFile(source)
					if err != nil || !bytes.Equal(before, after) {
						t.Fatalf("cached source changed: %v", err)
					}
				})
			}
		}
	}
}

func TestPrepareUSBModeRefusesFilesystemNeedingRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  uint16
		replay bool
	}{
		{name: "unclean", state: 0},
		{name: "recorded-errors", state: 3},
		{name: "orphan-recovery", state: 5},
		{name: "journal-replay", state: 1, replay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := modeFixture(t, "flashpkg-4k.ext4.gz", "")
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			sb := data[ext4SuperOffset : ext4SuperOffset+1024]
			binary.LittleEndian.PutUint16(sb[58:60], tc.state)
			if tc.replay {
				binary.LittleEndian.PutUint32(sb[96:100], binary.LittleEndian.Uint32(sb[96:100])|4)
			}
			// Keep the superblock valid: a checksum error must not mask acceptance
			// of a filesystem that records errors alongside EXT4_VALID_FS.
			binary.LittleEndian.PutUint32(sb[1020:1024], ^crc32.Checksum(sb[:1020], crc32.MakeTable(crc32.Castagnoli)))
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			temp := t.TempDir()
			if _, _, err := prepareUSBMode(source, temp, true); err == nil || !strings.Contains(err.Error(), "needs recovery") {
				t.Fatalf("unsafe filesystem: %v", err)
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(after, data) {
				t.Fatalf("source changed: %v", err)
			}
			files, err := os.ReadDir(temp)
			if err != nil || len(files) != 0 {
				t.Fatalf("temporary files created: %v, %v", files, err)
			}
		})
	}
}

func TestSingleEnumerationRequiresExactCapability(t *testing.T) {
	if hasSingleEnumeration(nil) || hasSingleEnumeration([]string{"usb-single-enumeration-v2"}) {
		t.Fatal("unknown capability selected single mode")
	}
	if !hasSingleEnumeration([]string{"future-capability", SingleEnumerationCapability}) {
		t.Fatal("advertised capability was missed")
	}
}

func TestVerifiedIdentityAdvertisesOptionalCapability(t *testing.T) {
	imagePath := modeFixture(t, "flashpkg-1k.ext4.gz", "")
	data, err := Ext4ReadFile(openFixture(t, "flashpkg-identity-1k.ext4.gz"), "flashpkg/device.json")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["capabilities"] = []string{SingleEnumerationCapability}
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, imagePath, "flashpkg/device.json", data)
	contents, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	stage := Stage2{TempDir: t.TempDir(), Out: io.Discard, ExpectedIdentity: identityExpectation,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			return os.WriteFile(req.Writer.DumpTo, contents, 0600)
		},
	}
	disk := UMSDisk{Serial: "12345678"}
	if _, err := stage.verifyDeviceIdentity(context.Background(), disk); err != nil || !stage.supportsSingle {
		t.Fatalf("capability handshake: %v, supportsSingle=%v", err, stage.supportsSingle)
	}
	stage.supportsSingle = false
	stage.ExpectedIdentity.ModuleSKU = "0004"
	if _, err := stage.verifyDeviceIdentity(context.Background(), disk); err == nil || stage.supportsSingle {
		t.Fatalf("capability adopted before identity validation: %v", err)
	}
	// The pre-change decoder ignored extra JSON fields.
	var oldIdentity struct {
		Protocol   string `json:"protocol"`
		SessionID  string `json:"session_id"`
		ModuleID   string `json:"module_id"`
		ModuleSKU  string `json:"module_sku"`
		CarrierID  string `json:"carrier_id"`
		CarrierSKU string `json:"carrier_sku"`
	}
	if err := json.Unmarshal(data, &oldIdentity); err != nil || oldIdentity.Protocol != DeviceIdentityProtocol || oldIdentity.ModuleSKU != identityExpectation.ModuleSKU {
		t.Fatalf("old identity decoder: %+v, %v", oldIdentity, err)
	}
}

func TestVerifyFlashPackageRejectsLostUSBMode(t *testing.T) {
	source := modeFixture(t, "flashpkg-4k.ext4.gz", "single\n")
	legacy, err := io.ReadAll(openFixture(t, "flashpkg-4k.ext4.gz"))
	if err != nil {
		t.Fatal(err)
	}
	stage := Stage2{FlashPackagePath: source, TempDir: t.TempDir(), Out: io.Discard,
		RunHelper: func(_ context.Context, req HelperRequest, _ func(int64, int64)) error {
			return os.WriteFile(req.Writer.DumpTo, legacy, 0600)
		},
	}
	if err := stage.verifyFlashPackage(context.Background(), UMSDisk{}); err == nil || !strings.Contains(err.Error(), "USB mode") {
		t.Fatalf("lost mode readback = %v", err)
	}
}
