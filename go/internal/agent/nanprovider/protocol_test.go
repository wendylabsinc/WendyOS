package nanprovider

import (
	"encoding/hex"
	"testing"
)

func TestPresenceValidation(t *testing.T) {
	encode := func(s string) string { return hex.EncodeToString([]byte(s)) }
	if asset, err := decodePresence(encode("wendy-nan:1:64:460"), 64); err != nil || asset != 460 {
		t.Fatalf("presence: %d %v", asset, err)
	}
	for _, bad := range []string{"wendy-nan:1:65:460", "wendy-nan:2:64:460", "wendy-nan:1:64:0", "wendy-nan:1:64:65535", "wendy-nan:1:64:460garbage", "wendy-nan:1:64:0460", ""} {
		if _, err := decodePresence(encode(bad), 64); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := decodePresence("not hex", 64); err == nil {
		t.Fatal("accepted nonhex")
	}
}
func TestNDPEventParsing(t *testing.T) {
	kind, values := parseEvent("IFNAME=nan0 <3>NAN-NDP-CONNECTED peer=aa:bb:cc:dd:ee:ff ndp_id=1 peer_ndi=00:11:22:33:44:55 ssi=\n")
	if kind != "NAN-NDP-CONNECTED" || values["ndp_id"] != "1" || values["peer_ndi"] != "00:11:22:33:44:55" {
		t.Fatalf("%s %+v", kind, values)
	}
}
