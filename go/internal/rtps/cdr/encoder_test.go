package cdr

import (
	"bytes"
	"math"
	"testing"
)

func TestEncoderAlignsEachFieldFromTheBodyStart(t *testing.T) {
	e := NewEncoder()
	e.Uint8(1)
	e.Uint32(2)
	e.Uint8(3)
	e.Float64(1.5)
	want := []byte{
		0x00, 0x01, 0x00, 0x00, // CDR_LE encapsulation header, not counted for alignment
		1, 0, 0, 0, // uint8, then padding to 4
		2, 0, 0, 0, // uint32
		3, 0, 0, 0, 0, 0, 0, 0, // uint8, then padding to 8
		0, 0, 0, 0, 0, 0, 0xf8, 0x3f, // float64 1.5
	}
	if got := e.Payload(); !bytes.Equal(got, want) {
		t.Fatalf("payload = % x\nwant      % x", got, want)
	}
}

func TestEncoderStringsCarryTheirTerminator(t *testing.T) {
	e := NewEncoder()
	e.String("ab")
	e.String("")
	want := []byte{
		0x00, 0x01, 0x00, 0x00,
		3, 0, 0, 0, 'a', 'b', 0, // length counts the NUL
		0,          // padding to 4 before the next length
		1, 0, 0, 0, // an empty string is still one NUL
		0,
	}
	if got := e.Payload(); !bytes.Equal(got, want) {
		t.Fatalf("payload = % x\nwant      % x", got, want)
	}
}

func TestEncoderOutputIsReadBackByTheDecoder(t *testing.T) {
	e := NewEncoder()
	e.Bool(true)
	e.Uint16(0xbeef)
	e.Int32(-7)
	e.Uint64(1 << 40)
	e.Float32(2.5)
	e.String("frame")
	e.Bytes([]byte{9, 8, 7})
	e.Float64(-0.125)

	d, err := NewDecoder(e.Payload())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.Bool(); err != nil || !v {
		t.Fatalf("bool = %v, %v", v, err)
	}
	if v, err := d.Uint16(); err != nil || v != 0xbeef {
		t.Fatalf("uint16 = %#x, %v", v, err)
	}
	if v, err := d.Int32(); err != nil || v != -7 {
		t.Fatalf("int32 = %d, %v", v, err)
	}
	if v, err := d.Uint64(); err != nil || v != 1<<40 {
		t.Fatalf("uint64 = %d, %v", v, err)
	}
	if v, err := d.Float32(); err != nil || v != 2.5 {
		t.Fatalf("float32 = %v, %v", v, err)
	}
	if v, err := d.String(); err != nil || v != "frame" {
		t.Fatalf("string = %q, %v", v, err)
	}
	if v, err := d.Bytes(); err != nil || !bytes.Equal(v, []byte{9, 8, 7}) {
		t.Fatalf("bytes = % x, %v", v, err)
	}
	if v, err := d.Uint64(); err != nil || math.Float64frombits(v) != -0.125 {
		t.Fatalf("float64 = %v, %v", math.Float64frombits(v), err)
	}
	if d.Remaining() != 0 {
		t.Fatalf("%d bytes left unread", d.Remaining())
	}
}
