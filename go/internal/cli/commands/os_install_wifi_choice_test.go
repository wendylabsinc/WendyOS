//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

// stubWifiChoice fakes the keychain and the Wi-Fi prompts of
// resolveWiFiCredentialsList and counts how often each is reached.
func stubWifiChoice(t *testing.T, interactive bool, keychainPW string) (keychain, prompts *int) {
	t.Helper()
	setInteractive(t, interactive)
	origKeychain, origSetup, origPassword := wifiKeychainPassword, confirmWifiSetup, promptWifiShortcutPassword
	t.Cleanup(func() {
		wifiKeychainPassword, confirmWifiSetup, promptWifiShortcutPassword = origKeychain, origSetup, origPassword
	})
	keychain, prompts = new(int), new(int)
	wifiKeychainPassword = func(string) (string, error) {
		*keychain++
		if keychainPW == "" {
			return "", errors.New("not in keychain")
		}
		return keychainPW, nil
	}
	confirmWifiSetup = func() (bool, error) { *prompts++; return false, nil }
	promptWifiShortcutPassword = func(string) (string, error) { *prompts++; return "typed-pw", nil }
	return keychain, prompts
}

func TestResolveWiFiRejectsContradictoryFlagsFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts wifiCLIOptions
		want string
	}{
		{"no-wifi with password", wifiCLIOptions{NoWifi: true, Password: "pw"}, "--no-wifi cannot be combined"},
		{"no-wifi with ssid", wifiCLIOptions{NoWifi: true, SSID: "Home"}, "--no-wifi cannot be combined"},
		{"no-wifi with entry", wifiCLIOptions{NoWifi: true, Entries: []string{"ssid=Home"}}, "--no-wifi cannot be combined"},
		{"password alone", wifiCLIOptions{Password: "pw"}, "--wifi-password requires --wifi-ssid"},
		{"malformed entry", wifiCLIOptions{Entries: []string{"ssid=Home", "priority=high,ssid=Cafe"}}, "invalid --wifi"},
	} {
		for _, interactive := range []bool{false, true} {
			keychain, prompts := stubWifiChoice(t, interactive, "")
			tc.opts.RequireChoice = true
			_, err := resolveWiFiCredentialsList(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !IsUsageError(err) {
				t.Errorf("%s (interactive=%v): %v; want a usage error containing %q", tc.name, interactive, err, tc.want)
			}
			if *keychain != 0 || *prompts != 0 {
				t.Errorf("%s: consulted the keychain %d and prompted %d times before rejecting the flags", tc.name, *keychain, *prompts)
			}
		}
	}
}

func TestBadWiFiEntryErrorsNeverEchoThePassword(t *testing.T) {
	stubWifiChoice(t, false, "")
	// Zq9 and Wv7 are pieces of one password; an unescaped comma splits it.
	for _, entry := range []string{
		"ssid=Home,password=Zq9Wv7,priority=high",
		"ssid=Home,password=Zq9Wv7,hidden=maybe",
		"ssid=Home,password=Zq9,Wv7",
		"ssid=Home,password=Zq9,x=Wv7",
		"ssid=Home,pasword=Zq9Wv7",
		"password=Zq9Wv7",
	} {
		opts := wifiCLIOptions{Entries: []string{"ssid=Cafe,password=Zq9Wv7", entry}, RequireChoice: true}
		_, resolveErr := resolveWiFiCredentialsList(opts)
		for _, err := range []error{validateWiFiFlags(opts), resolveErr} {
			if err == nil || !IsUsageError(err) || !strings.Contains(err.Error(), "invalid --wifi entry 2") {
				t.Fatalf("%q: %v; want a usage error naming entry 2", entry, err)
			}
			if strings.Contains(err.Error(), "Zq9") || strings.Contains(err.Error(), "Wv7") {
				t.Fatalf("%q: error %q echoes the password", entry, err)
			}
			if strings.HasPrefix(entry, "ssid=Home") && !strings.Contains(err.Error(), `ssid "Home"`) {
				t.Errorf("%q: error %q does not name the SSID", entry, err)
			}
		}
	}
}

func TestResolveWiFiUndecidedWithoutTerminal(t *testing.T) {
	_, prompts := stubWifiChoice(t, false, "")

	creds, err := resolveWiFiCredentialsList(wifiCLIOptions{RequireChoice: true})
	if !errors.Is(err, errWiFiUndecided) || creds != nil {
		t.Fatalf("got %v, %v; want errWiFiUndecided", creds, err)
	}
	for _, flag := range []string{"--wifi ssid=", "--wifi-ssid", "--no-wifi"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("error %q does not name %s", err, flag)
		}
	}
	if *prompts != 0 {
		t.Fatal("prompted without a terminal")
	}
}

func TestResolveWiFiExplicitNoneWithoutTerminal(t *testing.T) {
	stubWifiChoice(t, false, "")
	creds, err := resolveWiFiCredentialsList(wifiCLIOptions{NoWifi: true, RequireChoice: true})
	if err != nil || creds != nil {
		t.Fatalf("--no-wifi: got %v, %v; want nil, nil", creds, err)
	}
}

func TestResolveWiFiOptionalCallerKeepsNoneWithoutTerminal(t *testing.T) {
	// Callers without Wi-Fi flags (wendy run reinstalling Wendy Lite) cannot act on the error.
	stubWifiChoice(t, false, "")
	creds, err := resolveWiFiCredentialsList(wifiCLIOptions{})
	if err != nil || creds != nil {
		t.Fatalf("got %v, %v; want nil, nil", creds, err)
	}
}

func TestResolveWiFiSSIDWithoutPassword(t *testing.T) {
	t.Run("keychain has it", func(t *testing.T) {
		stubWifiChoice(t, false, "saved-pw")
		creds, err := resolveWiFiCredentialsList(wifiCLIOptions{SSID: "Home", RequireChoice: true})
		if err != nil || len(creds) != 1 || creds[0].Password != "saved-pw" {
			t.Fatalf("got %+v, %v; want the keychain password", creds, err)
		}
	})
	t.Run("no terminal and no keychain entry", func(t *testing.T) {
		keychain, prompts := stubWifiChoice(t, false, "")
		_, err := resolveWiFiCredentialsList(wifiCLIOptions{SSID: "Home", RequireChoice: true})
		if err == nil || !IsUsageError(err) {
			t.Fatalf("got %v; want a usage error instead of an open network", err)
		}
		for _, want := range []string{`"Home"`, "--wifi-password", "--wifi ssid=Home"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
		if *keychain != 1 || *prompts != 0 {
			t.Fatalf("keychain %d, prompts %d; want 1 and 0", *keychain, *prompts)
		}
	})
	t.Run("terminal asks", func(t *testing.T) {
		_, prompts := stubWifiChoice(t, true, "")
		creds, err := resolveWiFiCredentialsList(wifiCLIOptions{SSID: "Home", RequireChoice: true})
		if err != nil || len(creds) != 1 || creds[0].Password != "typed-pw" || *prompts != 1 {
			t.Fatalf("got %+v, %v after %d prompts; want the typed password", creds, err, *prompts)
		}
	})
	t.Run("open network by entry", func(t *testing.T) {
		keychain, _ := stubWifiChoice(t, false, "")
		creds, err := resolveWiFiCredentialsList(wifiCLIOptions{Entries: []string{"ssid=Cafe"}, RequireChoice: true})
		if err != nil || len(creds) != 1 || creds[0].Password != "" || *keychain != 0 {
			t.Fatalf("got %+v, %v; want one open network", creds, err)
		}
	})
}

// The open-network hint must survive a POSIX shell and parseWiFiEntry.
func TestOpenNetworkHintParsesBackToTheSSID(t *testing.T) {
	unquote := func(arg string) string {
		if inner, ok := strings.CutPrefix(arg, "'"); ok {
			return strings.ReplaceAll(strings.TrimSuffix(inner, "'"), `'\''`, "'")
		}
		return arg
	}
	for _, ssid := range []string{"Home", "My,Net", "a,,b", `Back\slash`, `Ends\`, `Lit\,Comma`, "Bob's Wi-Fi", "k=v", "Café 5G"} {
		stubWifiChoice(t, false, "")
		_, err := resolveWiFiCredentialsList(wifiCLIOptions{SSID: ssid, RequireChoice: true})
		if err == nil {
			t.Fatalf("%q: no error", ssid)
		}
		_, hint, ok := strings.Cut(err.Error(), "or --wifi ")
		hint, _, found := strings.Cut(hint, " for an open network")
		if !ok || !found {
			t.Fatalf("%q: no --wifi hint in %q", ssid, err)
		}
		c, perr := parseWiFiEntry(unquote(hint))
		if perr != nil || c.SSID != ssid || c.Password != "" {
			t.Errorf("%q: hint %s parses to %+v, %v", ssid, hint, c, perr)
		}
	}
	// parseWiFiEntry trims surrounding spaces, and control characters are not
	// echoed, so these get a placeholder rather than a hint for another network.
	for _, ssid := range []string{" Home", "Home\x1b[2J"} {
		if got := openWiFiFlag(ssid); got != "--wifi ssid=<name>" {
			t.Errorf("openWiFiFlag(%q) = %q; want the placeholder", ssid, got)
		}
	}
}

func TestResolveWiFiInteractiveDeclineIsExplicitNone(t *testing.T) {
	_, prompts := stubWifiChoice(t, true, "")
	creds, err := resolveWiFiCredentialsList(wifiCLIOptions{RequireChoice: true})
	if err != nil || creds != nil || *prompts != 1 {
		t.Fatalf("got %v, %v after %d prompts; want nil, nil after one question", creds, err, *prompts)
	}
}

// Past the Wi-Fi choice the Dragonwing path fetches only manifests before its
// next question; serveManifests fails the test on any other request.
func TestInstallCommandWithoutTerminalNeedsAWiFiChoiceBeforeDownloading(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LocalAppData", cache)
	stubSessionTerminals(t, false, false, nil)
	// An earlier test may have enabled telemetry, which would post through the stub.
	analytics.Disable()
	serveManifests(t, map[string]string{
		"manifests/master.json": `{"devices":{"dragonwing-iq-9075":{"latest":"1.0.0","manifest_path":"manifests/iq9.json"}}}`,
		"manifests/iq9.json":    `{"versions":{"1.0.0":{"qcomflash_path":"iq9/bundle.tar.gz","qcomflash_checksum":"abc123","qcomflash_size_bytes":1024}}}`,
	})
	run := func(args ...string) error {
		cmd := newOSInstallCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"--device-type", "dragonwing-iq-9075"}, args...))
		return cmd.Execute()
	}

	if err := run(); !errors.Is(err, errWiFiUndecided) {
		t.Fatalf("no Wi-Fi flag: %v; want errWiFiUndecided", err)
	}
	// Past the Wi-Fi choice, the run stops at the next question nobody can answer.
	if err := run("--no-wifi"); err == nil || errors.Is(err, errWiFiUndecided) || !strings.Contains(err.Error(), "pass --force") {
		t.Fatalf("--no-wifi: %v; want the Wi-Fi choice settled and the confirmation refused", err)
	}
}

// The Wi-Fi rule is checked before the manifest is read, which comes before
// elevation, drive selection and any download on every install path.
func TestInstallCommandWithoutTerminalChecksWiFiBeforeAnyWork(t *testing.T) {
	stubSessionTerminals(t, false, false, nil)
	analytics.Disable()
	serveManifests(t, nil) // any request fails the test
	run := func(args ...string) error {
		cmd := newOSInstallCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	for _, args := range [][]string{
		{"--device-type", "raspberry-pi-5", "--drive", "/dev/wendy-test-missing"},
		{"--device-type", "raspberry-pi-5", "--drive", "/dev/wendy-test-missing", "--force", "--device-name", "lab-pi"},
		{"--device-type", "jetson-orin-nano", "--force"},
		{"--device-type", "jetson-agx-thor"},
		{"--device-type", "dragonwing-iq-9075", "--pre-enroll=false"},
	} {
		if err := run(args...); !errors.Is(err, errWiFiUndecided) || !IsUsageError(err) {
			t.Errorf("%v: %v; want errWiFiUndecided as a usage error", args, err)
		}
	}
	// Writing a local image asks no first-boot questions.
	if err := run("/nonexistent/wendy-test.img", "/dev/wendy-test-missing"); err == nil || errors.Is(err, errWiFiUndecided) {
		t.Errorf("positional image and drive: %v; want the missing image reported", err)
	}
}

func TestInstallAsksWiFi(t *testing.T) {
	for deviceType, want := range map[string]bool{
		"raspberry-pi-5":     true,
		"jetson-agx-thor":    true,
		"dragonwing-iq-9075": true,
		"":                   false,
		linuxDesktopValue:    false,
		headlessMacValue:     false,
		"esp32-c6":           false,
	} {
		if got := installAsksWiFi(deviceType); got != want {
			t.Errorf("installAsksWiFi(%q) = %v; want %v", deviceType, got, want)
		}
	}
}

// Wendy Lite looks for the board before asking anything: with no board
// attached, the run reports that instead of reaching the Wi-Fi settings,
// which a malformed entry would otherwise fail first.
func TestInstallESP32FirmwareScansForBoardBeforeQuestions(t *testing.T) {
	stubSessionTerminals(t, false, false, nil)
	orig := resolveESP32InstallSerialPortsFn
	t.Cleanup(func() { resolveESP32InstallSerialPortsFn = orig })
	resolveESP32InstallSerialPortsFn = func() ([]discovery.SerialPortInfo, error) { return nil, nil }
	err := installESP32Firmware(context.Background(), false, "esp32c6_generic_native", discovery.SerialPortInfo{},
		wifiCLIOptions{Entries: []string{"not-key-value"}}, "", preEnrollOptions{mode: preEnrollSkip})
	if err == nil || !strings.Contains(err.Error(), "ESP32 not found") {
		t.Fatalf("no board: %v; want ESP32 not found before any Wi-Fi question", err)
	}
}
