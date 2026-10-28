package netwire

import (
	"bytes"
	"encoding/hex"
	"github.com/google/uuid"
	"net/netip"
	"testing"
)

func TestBinaryBytesAndStructuredIdentity(t *testing.T) {
	value := struct {
		Device uuid.UUID    `json:"device"`
		Prefix netip.Prefix `json:"prefix"`
		Data   []byte       `json:"data"`
	}{uuid.MustParse("00000000-0000-4000-8000-000000000004"), netip.MustParsePrefix("fd45:b891:567f::4/128"), []byte{0, 255, 1, 2}}
	wire, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte{0x44, 0, 255, 1, 2}) {
		t.Fatalf("missing native byte string: %s", hex.EncodeToString(wire))
	}
	actual := value
	actual.Device = uuid.Nil
	actual.Prefix = netip.Prefix{}
	actual.Data = nil
	if err = Unmarshal(wire, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Device != value.Device || actual.Prefix != value.Prefix || !bytes.Equal(actual.Data, value.Data) {
		t.Fatalf("round trip: %+v", actual)
	}
}
func TestCanonicalMapOrder(t *testing.T) {
	wire, err := Marshal(map[string]any{"data": []byte{0, 255}, "op": "gone"})
	if err != nil {
		t.Fatal(err)
	}
	// Length-first deterministic CBOR, text keys, native byte string.
	expected, _ := hex.DecodeString("a2626f7064676f6e6564646174614200ff")
	if !bytes.Equal(wire, expected) {
		t.Fatalf("%x != %x", wire, expected)
	}
}
