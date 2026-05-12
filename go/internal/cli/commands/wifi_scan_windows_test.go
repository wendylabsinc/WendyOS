//go:build windows

package commands

import (
	"testing"
)

func TestParseNetshNetworks(t *testing.T) {
	t.Run("single network with one BSSID", func(t *testing.T) {
		input := "Interface name : Wi-Fi\r\n" +
			"There are 1 networks currently visible.\r\n" +
			"\r\n" +
			"SSID 1 : HomeNet\r\n" +
			"    Network type            : Infrastructure\r\n" +
			"    Authentication          : WPA2-Personal\r\n" +
			"    Encryption              : CCMP\r\n" +
			"    BSSID 1                 : aa:bb:cc:dd:ee:ff\r\n" +
			"         Signal             : 78%\r\n" +
			"         Radio type         : 802.11n\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 1 {
			t.Fatalf("got %d networks, want 1: %+v", len(got), got)
		}
		if got[0].SSID != "HomeNet" {
			t.Errorf("SSID = %q, want HomeNet", got[0].SSID)
		}
		if got[0].SignalStrength != 78 {
			t.Errorf("SignalStrength = %d, want 78", got[0].SignalStrength)
		}
	})

	t.Run("multiple networks preserve order", func(t *testing.T) {
		input := "SSID 1 : Alpha\r\n" +
			"    BSSID 1 : aa:aa:aa:aa:aa:aa\r\n" +
			"         Signal : 50%\r\n" +
			"\r\n" +
			"SSID 2 : Bravo\r\n" +
			"    BSSID 1 : bb:bb:bb:bb:bb:bb\r\n" +
			"         Signal : 90%\r\n" +
			"\r\n" +
			"SSID 3 : Charlie\r\n" +
			"    BSSID 1 : cc:cc:cc:cc:cc:cc\r\n" +
			"         Signal : 25%\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 3 {
			t.Fatalf("got %d networks, want 3: %+v", len(got), got)
		}
		wantSSIDs := []string{"Alpha", "Bravo", "Charlie"}
		wantSignals := []int32{50, 90, 25}
		for i, n := range got {
			if n.SSID != wantSSIDs[i] {
				t.Errorf("got[%d].SSID = %q, want %q", i, n.SSID, wantSSIDs[i])
			}
			if n.SignalStrength != wantSignals[i] {
				t.Errorf("got[%d].SignalStrength = %d, want %d", i, n.SignalStrength, wantSignals[i])
			}
		}
	})

	t.Run("hidden SSID is skipped", func(t *testing.T) {
		input := "SSID 1 : \r\n" +
			"    BSSID 1 : aa:aa:aa:aa:aa:aa\r\n" +
			"         Signal : 60%\r\n" +
			"\r\n" +
			"SSID 2 : Visible\r\n" +
			"    BSSID 1 : bb:bb:bb:bb:bb:bb\r\n" +
			"         Signal : 40%\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 1 {
			t.Fatalf("got %d networks, want 1: %+v", len(got), got)
		}
		if got[0].SSID != "Visible" {
			t.Errorf("SSID = %q, want Visible", got[0].SSID)
		}
	})

	t.Run("missing Signal line yields zero", func(t *testing.T) {
		input := "SSID 1 : NoSig\r\n" +
			"    BSSID 1 : aa:aa:aa:aa:aa:aa\r\n" +
			"         Radio type : 802.11n\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 1 {
			t.Fatalf("got %d networks, want 1: %+v", len(got), got)
		}
		if got[0].SSID != "NoSig" {
			t.Errorf("SSID = %q, want NoSig", got[0].SSID)
		}
		if got[0].SignalStrength != 0 {
			t.Errorf("SignalStrength = %d, want 0", got[0].SignalStrength)
		}
	})

	t.Run("multiple BSSIDs take strongest signal", func(t *testing.T) {
		input := "SSID 1 : Roaming\r\n" +
			"    BSSID 1 : aa:aa:aa:aa:aa:aa\r\n" +
			"         Signal : 45%\r\n" +
			"    BSSID 2 : aa:aa:aa:aa:aa:bb\r\n" +
			"         Signal : 88%\r\n" +
			"    BSSID 3 : aa:aa:aa:aa:aa:cc\r\n" +
			"         Signal : 60%\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 1 {
			t.Fatalf("got %d networks, want 1: %+v", len(got), got)
		}
		if got[0].SignalStrength != 88 {
			t.Errorf("SignalStrength = %d, want 88 (strongest of 45/88/60)", got[0].SignalStrength)
		}
	})

	t.Run("BSSID prefix does not match SSID pattern", func(t *testing.T) {
		input := "SSID 1 : Net\r\n" +
			"    BSSID 1 : aa:aa:aa:aa:aa:aa\r\n" +
			"         Signal : 50%\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 1 {
			t.Fatalf("got %d networks, want 1 (BSSID line must not be parsed as SSID): %+v", len(got), got)
		}
		if got[0].SSID != "Net" {
			t.Errorf("SSID = %q, want Net (BSSID value bled into SSID)", got[0].SSID)
		}
	})

	t.Run("empty output", func(t *testing.T) {
		got := parseNetshNetworks("")
		if len(got) != 0 {
			t.Errorf("got %d networks, want 0", len(got))
		}
	})

	t.Run("output with no networks", func(t *testing.T) {
		input := "Interface name : Wi-Fi\r\n" +
			"There are 0 networks currently visible.\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 0 {
			t.Errorf("got %d networks, want 0", len(got))
		}
	})

	t.Run("SSID containing colon is preserved", func(t *testing.T) {
		input := "SSID 1 : Café: Wi-Fi\r\n" +
			"    BSSID 1 : aa:aa:aa:aa:aa:aa\r\n" +
			"         Signal : 70%\r\n"
		got := parseNetshNetworks(input)
		if len(got) != 1 {
			t.Fatalf("got %d networks, want 1", len(got))
		}
		if got[0].SSID != "Café: Wi-Fi" {
			t.Errorf("SSID = %q, want %q", got[0].SSID, "Café: Wi-Fi")
		}
	})
}
