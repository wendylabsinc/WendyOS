package configpartition

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// makeELF writes a minimal ELF header to a temp file and returns its path.
// machine is the e_machine value (little-endian uint16 at bytes 18-19).
// class is EI_CLASS (byte 4): 2 = 64-bit.
// data is EI_DATA (byte 5): 1 = little-endian, 0 = invalid.
func makeELF(t *testing.T, class byte, machine uint16) string {
	t.Helper()
	return makeELFWithData(t, class, 1, machine)
}

func makeELFWithData(t *testing.T, class, data byte, machine uint16) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-elf-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, 20)
	buf[0], buf[1], buf[2], buf[3] = 0x7f, 'E', 'L', 'F'
	buf[4] = class
	buf[5] = data
	buf[18] = byte(machine)
	buf[19] = byte(machine >> 8)
	if _, err := f.Write(buf); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestValidateELF_ValidArch(t *testing.T) {
	machine := elfMachineByArch[runtime.GOARCH]
	path := makeELF(t, 2, machine)
	if err := validateELF(path); err != nil {
		t.Fatalf("expected no error for valid ELF, got: %v", err)
	}
}

func TestValidateELF_BadMagic(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-elf-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("not an ELF file at all!!!!!")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := validateELF(f.Name()); err == nil {
		t.Fatal("expected error for non-ELF file")
	}
}

func TestValidateELF_32Bit(t *testing.T) {
	machine := elfMachineByArch[runtime.GOARCH]
	path := makeELF(t, 1, machine) // class=1 → 32-bit
	if err := validateELF(path); err == nil {
		t.Fatal("expected error for 32-bit ELF")
	}
}

func TestValidateELF_BigEndian(t *testing.T) {
	machine := elfMachineByArch[runtime.GOARCH]
	path := makeELFWithData(t, 2, 2, machine) // data=2 → big-endian
	if err := validateELF(path); err == nil {
		t.Fatal("expected error for big-endian ELF")
	}
}

func TestValidateELF_WrongArch(t *testing.T) {
	// Use the "other" architecture value so the test works on both arm64 and amd64.
	wrongMachine := elfMachineByArch["arm64"]
	if runtime.GOARCH == "arm64" {
		wrongMachine = elfMachineByArch["amd64"]
	}
	path := makeELF(t, 2, wrongMachine)
	if err := validateELF(path); err == nil {
		t.Fatal("expected error for wrong-arch ELF")
	}
}

func TestValidateELF_TooShort(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "short-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x7f, 'E', 'L', 'F'}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := validateELF(f.Name()); err == nil {
		t.Fatal("expected error for truncated ELF header")
	}
}

func TestParseINI_BasicSections(t *testing.T) {
	data := []byte("[wifi]\nssid = MyNet\npassword = hunter2\n")
	got := parseINI(data)
	if got["wifi"]["ssid"] != "MyNet" {
		t.Errorf("ssid = %q; want %q", got["wifi"]["ssid"], "MyNet")
	}
	if got["wifi"]["password"] != "hunter2" {
		t.Errorf("password = %q; want %q", got["wifi"]["password"], "hunter2")
	}
}

func TestParseINI_Comments(t *testing.T) {
	data := []byte("# top comment\n[wifi]\n; inline comment\nssid = Net\n")
	got := parseINI(data)
	if got["wifi"]["ssid"] != "Net" {
		t.Errorf("ssid = %q; want %q", got["wifi"]["ssid"], "Net")
	}
	if _, ok := got[""]; ok {
		t.Error("should not have empty section key")
	}
}

func TestParseINI_ValueWithEquals(t *testing.T) {
	data := []byte("[wifi]\npassword = p@ss=word\n")
	got := parseINI(data)
	if got["wifi"]["password"] != "p@ss=word" {
		t.Errorf("password = %q; want %q", got["wifi"]["password"], "p@ss=word")
	}
}

func TestParseINI_Empty(t *testing.T) {
	got := parseINI([]byte(""))
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

// TestParseINI_NonASCIISSID guards against byte-level corruption of UTF-8
// values: emoji bytes (0xF0..0xF4 + 0x80-0xBF continuation) never overlap with
// `=` (0x3D), `[` (0x5B), `]` (0x5D), `#` (0x23), or `;` (0x3B), so the parser
// must return them verbatim.
func TestParseINI_NonASCIISSID(t *testing.T) {
	cases := []struct {
		name, ssid string
	}{
		{"latin1", "café-wifi"},
		{"cjk", "東京 Wi-Fi"},
		{"emoji-4byte", "Read Only Internet \xf0\x9f\xab\xa5"},
		{"home-emoji", "Home \xf0\x9f\x8f\xa0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := []byte("[wifi]\nssid = " + c.ssid + "\n")
			got := parseINI(data)
			if got["wifi"]["ssid"] != c.ssid {
				t.Errorf("ssid = %q (% x); want %q (% x)",
					got["wifi"]["ssid"], []byte(got["wifi"]["ssid"]),
					c.ssid, []byte(c.ssid))
			}
		})
	}
}

func TestApplyBinaryUpdate_ValidBinary(t *testing.T) {
	dir := t.TempDir()
	installDir := t.TempDir()

	// Write a valid ELF to the config dir using the existing makeELF helper.
	machine := elfMachineByArch[runtime.GOARCH]
	src := filepath.Join(dir, "wendy-agent")
	// Copy the ELF bytes from makeELF into src manually so we can place it at the right name.
	elfPath := makeELF(t, 2, machine)
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o755); err != nil {
		t.Fatal(err)
	}

	installPath := filepath.Join(installDir, "wendy-agent")
	logger, _ := zap.NewDevelopment()

	updated := applyBinaryUpdate(logger, dir, installPath)
	if !updated {
		t.Fatal("expected applyBinaryUpdate to return true")
	}

	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("installed binary not found: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode = %o; want exec bits set", info.Mode().Perm())
	}
	got, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("reading installed binary: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("installed binary content does not match source")
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source binary should be deleted after install")
	}
}

func TestApplyBinaryUpdate_InvalidELF(t *testing.T) {
	dir := t.TempDir()
	installDir := t.TempDir()

	src := filepath.Join(dir, "wendy-agent")
	if err := os.WriteFile(src, []byte("not an elf"), 0o755); err != nil {
		t.Fatal(err)
	}

	installPath := filepath.Join(installDir, "wendy-agent")
	logger, _ := zap.NewDevelopment()

	updated := applyBinaryUpdate(logger, dir, installPath)
	if updated {
		t.Fatal("expected applyBinaryUpdate to return false for invalid ELF")
	}

	// Source should be deleted (bad binary, don't retry).
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("invalid binary should be deleted from config partition")
	}
}

func TestApplyBinaryUpdate_NoBinary(t *testing.T) {
	dir := t.TempDir()
	installDir := t.TempDir()
	installPath := filepath.Join(installDir, "wendy-agent")
	logger, _ := zap.NewDevelopment()

	updated := applyBinaryUpdate(logger, dir, installPath)
	if updated {
		t.Fatal("expected false when no binary present")
	}
}

func TestApplyWendyConf_DeletesFileAfterApply(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "wendy.conf")
	// Empty ssid so nmcli is never called, but the file deletion path is exercised.
	if err := os.WriteFile(conf, []byte("[wifi]\nssid = \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	applyWendyConf(logger, dir)

	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Error("wendy.conf should be deleted after applyWendyConf")
	}
}

func TestApplyWendyConf_NoFile(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	// Should return without panic when file doesn't exist.
	applyWendyConf(logger, t.TempDir())
}

func TestValidDeviceName(t *testing.T) {
	valid := []string{
		"abc",
		"brave-dolphin",
		"my-device-1",
		"a23",
		strings.Repeat("a", maxDeviceNameLen), // at the length cap
	}
	for _, name := range valid {
		if !validDeviceName(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}

	invalid := []string{
		"",
		"ab",                                    // too short
		"1abc",                                  // starts with digit
		"-abc",                                  // starts with hyphen
		"ABC",                                   // uppercase
		"has space",                             // space
		strings.Repeat("a", maxDeviceNameLen+1), // one over the cap
		"valid_but_underscore",                  // underscore not allowed
		"trailing-hyphen-",                      // trailing hyphen -> invalid derived hostname
	}
	for _, name := range invalid {
		if validDeviceName(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}

	// The cap exists so the derived "wendyos-<name>" hostname stays within the
	// 63-octet RFC 1035 label limit (WDY-1518).
	if maxDeviceNameLen != 55 {
		t.Errorf("maxDeviceNameLen = %d; want 55", maxDeviceNameLen)
	}
	if got := len("wendyos-" + strings.Repeat("a", maxDeviceNameLen)); got > 63 {
		t.Errorf("derived hostname label is %d octets; exceeds the RFC 1035 limit of 63", got)
	}
}

func TestApplyDeviceName_InvalidName(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	if err := applyDeviceName(logger, "BAD NAME"); err == nil {
		t.Error("expected error for invalid device name")
	}
}

func TestApplyPreProvisioning_Success(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()

	state := `{"enrolled":true,"cloudHost":"cloud.wendy.dev","orgId":1,"assetId":42,"keyPem":"fake-key","certPem":"fake-cert","chainPem":"fake-chain"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(filepath.Join(cfgDir, "provisioning.json")); !os.IsNotExist(err) {
		t.Error("source provisioning.json should be deleted after apply")
	}

	got, err := os.ReadFile(filepath.Join(configPath, "provisioning.json"))
	if err != nil {
		t.Fatalf("provisioning.json not written to configPath: %v", err)
	}
	if string(got) != state {
		t.Errorf("provisioning.json content = %q; want %q", got, state)
	}

	for _, name := range []string{"device-key.pem", "device.pem", "ca.pem"} {
		if _, err := os.Stat(filepath.Join(configPath, name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}

	info, err := os.Stat(filepath.Join(configPath, "device-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("device-key.pem mode = %o; want 0600", info.Mode().Perm())
	}

	info, err = os.Stat(filepath.Join(configPath, "device.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("device.pem mode = %o; want 0644", info.Mode().Perm())
	}

	info, err = os.Stat(filepath.Join(configPath, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("ca.pem mode = %o; want 0644", info.Mode().Perm())
	}

	if _, err := os.Stat(filepath.Join(configPath, ".provisioned")); err != nil {
		t.Error(".provisioned marker not written")
	}
}

func TestApplyPreProvisioning_NoFile(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, t.TempDir(), t.TempDir()) // must not panic
}

func TestApplyPreProvisioning_MalformedJSON(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()
	srcPath := filepath.Join(cfgDir, "provisioning.json")
	if err := os.WriteFile(srcPath, []byte("not json {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Error("malformed source should be deleted")
	}
	if _, err := os.Stat(filepath.Join(configPath, "provisioning.json")); !os.IsNotExist(err) {
		t.Error("provisioning.json must not be written for malformed input")
	}
}

func TestApplyPreProvisioning_IncompleteState(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()
	srcPath := filepath.Join(cfgDir, "provisioning.json")
	// Missing keyPem — should be rejected.
	if err := os.WriteFile(srcPath, []byte(`{"enrolled":true,"cloudHost":"cloud.wendy.dev","certPem":"cert"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Error("incomplete source should be deleted")
	}
	if _, err := os.Stat(filepath.Join(configPath, "provisioning.json")); !os.IsNotExist(err) {
		t.Error("provisioning.json must not be written for incomplete input")
	}
}

func TestApplyPreProvisioning_CreatesConfigDir(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "subdir", "wendy-agent")

	state := `{"enrolled":true,"cloudHost":"cloud.wendy.dev","orgId":1,"assetId":42,"keyPem":"k","certPem":"c","chainPem":"ch"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, _ := zap.NewDevelopment()
	applyPreProvisioning(logger, cfgDir, configPath)

	if _, err := os.Stat(filepath.Join(configPath, "provisioning.json")); err != nil {
		t.Errorf("configPath should be created automatically: %v", err)
	}
}

const avahiServiceTemplate = `<?xml version="1.0" standalone='no'?>
<service-group>
  <name replace-wildcards="yes">WendyOS on %h</name>
  <service>
    <type>_wendyos._udp</type>
    <port>50051</port>
  </service>
  <service>
    <type>_ssh._tcp</type>
    <port>22</port>
  </service>
</service-group>
`

func TestUpdateWendyOSServicePort_Provisioned(t *testing.T) {
	out := updateWendyOSServicePort(avahiServiceTemplate, 50052, true, 0, 0)

	if !strings.Contains(out, "<port>50052</port>") {
		t.Errorf("expected wendyos port updated to 50052:\n%s", out)
	}
	if !strings.Contains(out, "<txt-record>tls=true</txt-record>") {
		t.Errorf("expected tls=true TXT record:\n%s", out)
	}
	// The unrelated SSH block must be left untouched.
	if !strings.Contains(out, "<port>22</port>") {
		t.Errorf("ssh port should be untouched:\n%s", out)
	}
}

func TestUpdateWendyOSServicePort_Unprovisioned(t *testing.T) {
	// Start from a provisioned advertisement and revert it.
	provisioned := updateWendyOSServicePort(avahiServiceTemplate, 50052, true, 0, 0)
	out := updateWendyOSServicePort(provisioned, 50051, false, 0, 0)

	if !strings.Contains(out, "<port>50051</port>") {
		t.Errorf("expected wendyos port reverted to 50051:\n%s", out)
	}
	if !strings.Contains(out, "<txt-record>tls=false</txt-record>") {
		t.Errorf("expected tls=false TXT record after revert:\n%s", out)
	}
	if strings.Contains(out, "tls=true") {
		t.Errorf("tls=true should have been replaced:\n%s", out)
	}
}

func TestUpdateWendyOSServicePort_ProvisionedWithAssetID(t *testing.T) {
	out := updateWendyOSServicePort(avahiServiceTemplate, 50052, true, 215, 0)

	if !strings.Contains(out, "<txt-record>assetid=215</txt-record>") {
		t.Errorf("expected assetid=215 TXT record:\n%s", out)
	}
}

func TestUpdateWendyOSServicePort_UnprovisioningRemovesAssetID(t *testing.T) {
	provisioned := updateWendyOSServicePort(avahiServiceTemplate, 50052, true, 215, 0)
	if !strings.Contains(provisioned, "assetid=215") {
		t.Fatalf("test setup: expected assetid=215 to be present:\n%s", provisioned)
	}

	out := updateWendyOSServicePort(provisioned, 50051, false, 0, 0)

	if strings.Contains(out, "assetid=") {
		t.Errorf("expected assetid TXT record to be removed on unprovisioning:\n%s", out)
	}
	// SSH block still untouched.
	if !strings.Contains(out, "<port>22</port>") {
		t.Errorf("ssh port should be untouched:\n%s", out)
	}
}

func TestUpdateWendyOSServicePort_UpdatesExistingAssetID(t *testing.T) {
	provisioned := updateWendyOSServicePort(avahiServiceTemplate, 50052, true, 215, 0)
	out := updateWendyOSServicePort(provisioned, 50052, true, 999, 0)

	if !strings.Contains(out, "<txt-record>assetid=999</txt-record>") {
		t.Errorf("expected assetid updated to 999:\n%s", out)
	}
	if strings.Contains(out, "assetid=215") {
		t.Errorf("old assetid=215 should have been replaced:\n%s", out)
	}
}

func TestUpdateAvahiService_ProvisioningWritesAssetIDTXTRecord(t *testing.T) {
	dir := t.TempDir()
	serviceFile := filepath.Join(dir, "wendyos-mdns.service")
	if err := os.WriteFile(serviceFile, []byte(avahiServiceTemplate), 0o644); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	updateAvahiService(logger, dir, 50052, true, 215, 0)

	got, err := os.ReadFile(serviceFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "<txt-record>assetid=215</txt-record>") {
		t.Errorf("expected assetid=215 TXT record written to service file:\n%s", got)
	}
	if !strings.Contains(string(got), "<port>50052</port>") {
		t.Errorf("expected port updated to 50052:\n%s", got)
	}
}

func TestUpdateAvahiService_UnprovisioningRemovesAssetID(t *testing.T) {
	dir := t.TempDir()
	serviceFile := filepath.Join(dir, "wendyos-mdns.service")
	provisioned := updateWendyOSServicePort(avahiServiceTemplate, 50052, true, 215, 0)
	if err := os.WriteFile(serviceFile, []byte(provisioned), 0o644); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	updateAvahiService(logger, dir, 50051, false, 0, 0)

	got, err := os.ReadFile(serviceFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "assetid=") {
		t.Errorf("expected assetid TXT record removed from service file:\n%s", got)
	}
}

func TestApplyClockFloor_CreatesConfigDir(t *testing.T) {
	cfgDir := t.TempDir()
	// Reproduces WDY-1868: on a fresh (or not-yet-provisioned) image the agent
	// config dir does not exist, and the floor copy must create it rather than
	// fail — without the floor a no-RTC device boots with a months-old clock.
	configPath := filepath.Join(t.TempDir(), "subdir", "wendy-agent")

	// 8-byte big-endian Unix-seconds payload (FloorBytes format); the value is
	// arbitrary — the test only asserts a byte-for-byte copy.
	payload := []byte{0, 0, 0, 0, 0x68, 0x6e, 0xda, 0x80}
	if err := os.WriteFile(filepath.Join(cfgDir, "clock_floor"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	applyClockFloor(logger, cfgDir, configPath)

	got, err := os.ReadFile(filepath.Join(configPath, "clock_floor"))
	if err != nil {
		t.Fatalf("clock_floor not written to configPath: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("clock_floor content = %v, want %v", got, payload)
	}

	if info, err := os.Stat(filepath.Join(configPath, "clock_floor")); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("clock_floor mode = %o, want 0600", perm)
	}
	if info, err := os.Stat(configPath); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 0700", perm)
	}
}

func TestApplyClockFloor_NoFile(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	// Should return without side effects when the partition has no floor file.
	configPath := filepath.Join(t.TempDir(), "wendy-agent")
	applyClockFloor(logger, t.TempDir(), configPath)
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Error("configPath should not be created when there is no clock_floor to copy")
	}
}

func TestUpdateOrgIDTXTRecord(t *testing.T) {
	block := "  <service>\n    <type>_wendyos._udp</type>\n  </service>"
	got := updateOrgIDTXTRecord(block, 42)
	if !strings.Contains(got, "<txt-record>orgid=42</txt-record>") {
		t.Fatalf("orgid record not inserted: %q", got)
	}
	// Idempotent replace, not duplicate.
	got2 := updateOrgIDTXTRecord(got, 7)
	if strings.Count(got2, "<txt-record>orgid=") != 1 || !strings.Contains(got2, "orgid=7") {
		t.Fatalf("orgid record not replaced idempotently: %q", got2)
	}
}
