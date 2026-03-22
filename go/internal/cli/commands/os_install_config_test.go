package commands

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOSInstallWiFiFlag(t *testing.T) {
	t.Run("secured", func(t *testing.T) {
		got, err := parseOSInstallWiFiFlag("HomeNet=secret")
		if err != nil {
			t.Fatalf("parseOSInstallWiFiFlag: %v", err)
		}
		if got.SSID != "HomeNet" || got.Password != "secret" {
			t.Fatalf("unexpected wifi parse result: %+v", got)
		}
	})

	t.Run("open", func(t *testing.T) {
		got, err := parseOSInstallWiFiFlag("CafeWiFi")
		if err != nil {
			t.Fatalf("parseOSInstallWiFiFlag: %v", err)
		}
		if got.SSID != "CafeWiFi" || got.Password != "" {
			t.Fatalf("unexpected wifi parse result: %+v", got)
		}
	})
}

func TestValidateOSInstallConfig(t *testing.T) {
	cfg := &osInstallConfig{
		Hostname: "wendyos-sunny-otter",
		WiFi: []osInstallWiFiCredential{
			{SSID: "HomeNet", Password: "secret"},
		},
		SSH: osInstallSSHConfig{
			Mode:          "key",
			Username:      "wendy",
			AuthorizedKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockKey test@example.com",
		},
	}

	if err := validateOSInstallConfig(cfg); err != nil {
		t.Fatalf("validateOSInstallConfig: %v", err)
	}
}

func TestRenderWiFiConnectionProfile(t *testing.T) {
	profile := renderWiFiConnectionProfile(osInstallWiFiCredential{
		SSID:     "HomeNet",
		Password: "secret",
	})

	required := []string{
		"[connection]",
		"type=wifi",
		"ssid=HomeNet",
		"[wifi-security]",
		"psk=secret",
	}
	for _, needle := range required {
		if !strings.Contains(profile, needle) {
			t.Fatalf("profile missing %q:\n%s", needle, profile)
		}
	}
}

func TestStageOSInstallPayload(t *testing.T) {
	tempDir := t.TempDir()

	agentPath := filepath.Join(tempDir, "wendy-agent")
	if err := os.WriteFile(agentPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agent): %v", err)
	}

	certsZip := filepath.Join(tempDir, "certs.zip")
	if err := writeTestZip(certsZip, map[string]string{
		"device.pem":     "cert",
		"device-key.pem": "key",
	}); err != nil {
		t.Fatalf("writeTestZip: %v", err)
	}

	payloadRoot, err := stageOSInstallPayload(&osInstallConfig{
		Hostname:    "wendyos-sunny-otter",
		AgentBinary: agentPath,
		CertsZip:    certsZip,
		WiFi: []osInstallWiFiCredential{
			{SSID: "HomeNet", Password: "secret"},
		},
		SSH: osInstallSSHConfig{
			Mode:          "key",
			Username:      "wendy",
			AuthorizedKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockKey test@example.com",
		},
	})
	if err != nil {
		t.Fatalf("stageOSInstallPayload: %v", err)
	}
	defer os.RemoveAll(payloadRoot)

	payloadDir := filepath.Join(payloadRoot, osInstallPayloadDirName)
	expectedFiles := []string{
		filepath.Join(payloadDir, "config.json"),
		filepath.Join(payloadDir, "network"),
		filepath.Join(payloadDir, "assets", "wendy-agent"),
		filepath.Join(payloadDir, "certs", "device.pem"),
		filepath.Join(payloadDir, "ssh", "authorized_keys"),
	}
	for _, path := range expectedFiles {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected payload path %s: %v", path, err)
		}
	}
}

func TestRandomFriendlyHostname(t *testing.T) {
	got := randomFriendlyHostname()
	if !strings.HasPrefix(got, "wendyos-") {
		t.Fatalf("randomFriendlyHostname() = %q; want prefix wendyos-", got)
	}
	if !osInstallHostnameRE.MatchString(got) {
		t.Fatalf("randomFriendlyHostname() = %q; does not match hostname validator", got)
	}
}

func TestLinuxDeviceRecommendation(t *testing.T) {
	if got := linuxDeviceRecommendation("jetson-orin-nano"); !strings.Contains(got, "NVMe") {
		t.Fatalf("linuxDeviceRecommendation(jetson-orin-nano) = %q; want NVMe note", got)
	}
	if got := linuxDeviceRecommendation("raspberry-pi-5"); !strings.Contains(got, "microSD") || !strings.Contains(got, "NVMe") {
		t.Fatalf("linuxDeviceRecommendation(raspberry-pi-5) = %q; want microSD/NVMe note", got)
	}
	if got := linuxDeviceRecommendation("test-device"); got != "" {
		t.Fatalf("linuxDeviceRecommendation(test-device) = %q; want empty string", got)
	}
}

func TestRenderLinuxImageAdvisory(t *testing.T) {
	jetson := renderLinuxImageAdvisory("jetson-orin-nano", "Jetson Orin Nano", &imageInfo{
		DownloadURL: "https://example.com/wendyos-image-jetson-orin-nano-devkit-nvme-wendyos.img.zip",
	})
	if !strings.Contains(jetson, "NVMe") || !strings.Contains(jetson, "recovery tegraflash") {
		t.Fatalf("renderLinuxImageAdvisory(jetson-orin-nano) = %q; want NVMe + recovery guidance", jetson)
	}

	pi := renderLinuxImageAdvisory("raspberry-pi-5", "Raspberry Pi 5", nil)
	if !strings.Contains(pi, "microSD") || !strings.Contains(pi, "NVMe") {
		t.Fatalf("renderLinuxImageAdvisory(raspberry-pi-5) = %q; want microSD/NVMe guidance", pi)
	}
}

func writeTestZip(path string, files map[string]string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	writer := zip.NewWriter(out)
	for name, contents := range files {
		entry, err := writer.Create(name)
		if err != nil {
			writer.Close()
			return err
		}
		if _, err := entry.Write([]byte(contents)); err != nil {
			writer.Close()
			return err
		}
	}
	return writer.Close()
}
