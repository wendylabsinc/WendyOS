package netwire

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sync"
	"testing"

	"github.com/google/uuid"
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

func TestCompactFieldsKeepTextAndBinaryValues(t *testing.T) {
	type message struct {
		Name string `json:"name" cbor:"n"`
		Data []byte `json:"data" cbor:"d"`
	}
	wire, err := Marshal(message{Name: "body"})
	if err != nil {
		t.Fatal(err)
	}
	// Compact names are explicit schema tags; text values are never rewritten,
	// and absent binary data uses an empty byte string instead of null.
	expected, _ := hex.DecodeString("a2616440616e64626f6479")
	if !bytes.Equal(wire, expected) {
		t.Fatalf("wire = %x, want %x", wire, expected)
	}
	var fields map[string]RawMessage
	if err := Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if Key("name") != "n" || Key("data") != "d" || Key("futureField") != "futureField" {
		t.Fatal("semantic key lookup changed")
	}
}

func TestRawSignedBodySurvivesEnvelope(t *testing.T) {
	// A foreign body can have a different map order. Its exact bytes remain
	// signature input even when its enclosing map is canonically encoded.
	body, _ := hex.DecodeString("a2616202616101")
	wire, err := Marshal(struct {
		Body RawMessage `cbor:"b"`
	}{body})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Body RawMessage `cbor:"b"`
	}
	if err := UnmarshalSigned(wire, &envelope); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(envelope.Body, body) {
		t.Fatalf("signed body changed: %x", envelope.Body)
	}
	// Decoded raw data owns its storage and survives reusing the input buffer.
	clear(wire)
	if !bytes.Equal(envelope.Body, body) {
		t.Fatal("signed body aliases input buffer")
	}
}

func TestConcurrentCodecModes(t *testing.T) {
	type message struct {
		Name string `cbor:"n"`
		Data []byte `cbor:"d"`
	}
	want := message{Name: "peer", Data: []byte{0, 255, 1}}
	var wg sync.WaitGroup
	errors := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			for range 32 {
				wire, err := Marshal(want)
				if err != nil {
					errors <- err
					return
				}
				for _, decode := range []func([]byte, any) error{
					Unmarshal, UnmarshalSigned,
					func(data []byte, value any) error { return UnmarshalExtensibleArtifact(data, value, 4096) },
				} {
					var got message
					if err := decode(wire, &got); err != nil {
						errors <- err
						return
					}
					if got.Name != want.Name || !bytes.Equal(got.Data, want.Data) {
						errors <- fmt.Errorf("round trip = %+v", got)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
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
