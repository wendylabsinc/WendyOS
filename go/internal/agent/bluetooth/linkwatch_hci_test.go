package bluetooth

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// hexBytes parses space-separated hex bytes, e.g. "04 3e 0a".
func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestDecodeHCIEvent(t *testing.T) {
	tests := []struct {
		name string
		pkt  string
		want hciEvent
	}{
		{
			name: "LE connection complete, central",
			pkt:  "04 3e 13 01 00 10 00 00 00 ff ee dd cc bb aa 06 00 00 00 2c 01 00",
			want: hciEvent{Kind: hciConnComplete, Handle: 0x0010, Central: true, Params: connParams{Interval: 6, Latency: 0, Timeout: 300}},
		},
		{
			name: "LE connection complete, peripheral",
			pkt:  "04 3e 13 01 00 10 00 01 00 ff ee dd cc bb aa 06 00 00 00 2c 01 00",
			want: hciEvent{Kind: hciConnComplete, Handle: 0x0010, Central: false, Params: connParams{Interval: 6, Latency: 0, Timeout: 300}},
		},
		{
			name: "LE enhanced connection complete",
			pkt: "04 3e 1f 0a 00 10 00 00 00 ff ee dd cc bb aa" +
				" 00 00 00 00 00 00 00 00 00 00 00 00 18 00 04 00 f4 01 00",
			want: hciEvent{Kind: hciConnComplete, Handle: 0x0010, Central: true, Params: connParams{Interval: 24, Latency: 4, Timeout: 500}},
		},
		{
			name: "LE connection update complete",
			pkt:  "04 3e 0a 03 00 10 00 06 00 00 00 32 00",
			want: hciEvent{Kind: hciConnUpdateComplete, Handle: 0x0010, Params: connParams{Interval: 6, Latency: 0, Timeout: 50}},
		},
		{
			name: "update complete masks the handle's flag bits",
			pkt:  "04 3e 0a 03 00 10 20 06 00 00 00 32 00",
			want: hciEvent{Kind: hciConnUpdateComplete, Handle: 0x0010, Params: connParams{Interval: 6, Latency: 0, Timeout: 50}},
		},
		{
			name: "update complete with an error status",
			pkt:  "04 3e 0a 03 3b 10 00 06 00 00 00 2c 01",
			want: hciEvent{Kind: hciConnUpdateComplete, Status: 0x3b, Handle: 0x0010, Params: connParams{Interval: 6, Latency: 0, Timeout: 300}},
		},
		{
			name: "disconnection complete",
			pkt:  "04 05 04 00 10 00 08",
			want: hciEvent{Kind: hciDisconnComplete, Handle: 0x0010, Reason: 0x08},
		},
		{
			name: "command status",
			pkt:  "04 0f 04 3a 01 13 20",
			want: hciEvent{Kind: hciCmdStatus, Status: 0x3a, Opcode: 0x2013},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := decodeHCIEvent(hexBytes(t, tt.pkt))
			if err != nil || !ok {
				t.Fatalf("decodeHCIEvent = ok %v, err %v; want a decoded event", ok, err)
			}
			if got != tt.want {
				t.Errorf("decodeHCIEvent = %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeHCIEvent_IgnoresPacketsItDoesNotUse(t *testing.T) {
	for name, pkt := range map[string]string{
		"command complete":   "04 0e 04 01 13 20 00",
		"advertising report": "04 3e 02 02 00",
		"ACL data":           "02 10 00 04 00 00 00 00 00",
		"empty":              "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok, err := decodeHCIEvent(hexBytes(t, pkt)); ok || err != nil {
				t.Errorf("decodeHCIEvent = ok %v, err %v; want ignored", ok, err)
			}
		})
	}
}

func TestDecodeHCIEvent_RejectsTruncatedPackets(t *testing.T) {
	for name, pkt := range map[string]string{
		"header only":                       "04 3e",
		"shorter than declared":             "04 3e 0a 03 00 10",
		"connection complete too short":     "04 3e 05 01 00 10 00 00",
		"enhanced complete missing timeout": "04 3e 13 0a 00 10 00 00 00 ff ee dd cc bb aa 06 00 00 00 2c 01 00",
		"update complete too short":         "04 3e 05 03 00 10 00 06",
		"disconnection complete too short":  "04 05 02 00 10",
		"command status too short":          "04 0f 02 00 01",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeHCIEvent(hexBytes(t, pkt)); !errors.Is(err, errShortHCIPacket) {
				t.Errorf("decodeHCIEvent err = %v; want errShortHCIPacket", err)
			}
		})
	}
}

func TestEncodeLEConnUpdate_PrototypeValues(t *testing.T) {
	// The values the hcitool prototype on the car used: handle 0x0010,
	// 7.5 ms interval, latency 0, 500 ms timeout.
	got := encodeLEConnUpdate(0x0010, connUpdate{IntervalMin: 6, IntervalMax: 6, Latency: 0, Timeout: 50})
	want := hexBytes(t, "01 13 20 0e 10 00 06 00 06 00 00 00 32 00 00 00 00 00")
	if !bytes.Equal(got, want) {
		t.Errorf("encodeLEConnUpdate = % x; want % x", got, want)
	}
}

func TestEncodeHCIFilter(t *testing.T) {
	want := hexBytes(t, "10 00 00 00 20 80 00 00 00 00 00 40 13 20 00 00")
	if got := encodeHCIFilter(); !bytes.Equal(got, want) {
		t.Errorf("encodeHCIFilter = % x; want % x", got, want)
	}
}

func TestParseConnList(t *testing.T) {
	buf := hexBytes(t, "00 00 02 00"+
		" 10 00 ff ee dd cc bb aa 80 01 01 00 01 00 00 00"+ // LE, central
		" 0b 00 66 55 44 33 22 11 01 00 01 00 00 00 00 00"+ // Classic, peripheral
		" 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00") // unused slot
	got, err := parseConnList(buf)
	if err != nil {
		t.Fatalf("parseConnList: %v", err)
	}
	want := []connInfo{
		{Handle: 0x0010, Address: "AA:BB:CC:DD:EE:FF", LinkType: hciLinkLE, Central: true},
		{Handle: 0x000b, Address: "11:22:33:44:55:66", LinkType: hciLinkACL, Central: false},
	}
	if len(got) != len(want) {
		t.Fatalf("parseConnList = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v; want %+v", i, got[i], want[i])
		}
	}
	if _, err := parseConnList(hexBytes(t, "00 00 02 00 10 00")); !errors.Is(err, errShortHCIPacket) {
		t.Errorf("parseConnList(truncated) err = %v; want errShortHCIPacket", err)
	}
}

func TestDevInfoAddress(t *testing.T) {
	buf := make([]byte, hciDevInfoSize)
	copy(buf[10:], hexBytes(t, "66 55 44 33 22 11"))
	if got, err := devInfoAddress(buf); err != nil || got != "11:22:33:44:55:66" {
		t.Errorf("devInfoAddress = %q, %v; want 11:22:33:44:55:66", got, err)
	}
}

func TestDisconnectReasonTextAndLinkTypeName(t *testing.T) {
	for code, want := range map[uint8]string{
		0x08: "connection timeout",
		0x13: "remote user terminated connection",
		0x15: "remote device terminated connection: power off",
		0x16: "connection terminated by local host",
		0x99: "unknown",
	} {
		if got := disconnectReasonText(code); got != want {
			t.Errorf("disconnectReasonText(0x%02x) = %q; want %q", code, got, want)
		}
	}
	for typ, want := range map[uint8]string{
		hciLinkLE: "le", hciLinkACL: "classic", hciLinkESCO: "sco", hciLinkUnknown: "unknown", 0x83: "0x83",
	} {
		if got := linkTypeName(typ); got != want {
			t.Errorf("linkTypeName(0x%02x) = %q; want %q", typ, got, want)
		}
	}
}
