package cdr

import (
	"errors"
	"testing"
)

// le builds a little-endian CDR payload: encapsulation header + body.
func le(body ...byte) []byte {
	return append([]byte{0x00, 0x01, 0x00, 0x00}, body...)
}

// be builds a big-endian CDR payload.
func be(body ...byte) []byte {
	return append([]byte{0x00, 0x00, 0x00, 0x00}, body...)
}

func TestNewDecoder_RejectsTruncatedHeader(t *testing.T) {
	if _, err := NewDecoder([]byte{0x00, 0x01}); !errors.Is(err, ErrShort) {
		t.Fatalf("err = %v; want ErrShort", err)
	}
}

func TestNewDecoder_RejectsUnknownEncapsulation(t *testing.T) {
	if _, err := NewDecoder([]byte{0x00, 0x7f, 0x00, 0x00}); err == nil {
		t.Fatal("expected an error for an unknown representation identifier")
	}
}

func TestDecoder_LittleEndianPrimitives(t *testing.T) {
	d, err := NewDecoder(le(0x2a, 0x00, 0x34, 0x12, 0x78, 0x56, 0x34, 0x12))
	if err != nil {
		t.Fatal(err)
	}
	// uint8 at 0, then uint16 aligned to 2 (skipping one pad byte), then
	// uint32 aligned to 4.
	if v, err := d.Uint8(); err != nil || v != 0x2a {
		t.Fatalf("Uint8 = %v, %v; want 42, nil", v, err)
	}
	if v, err := d.Uint16(); err != nil || v != 0x1234 {
		t.Fatalf("Uint16 = %#x, %v; want 0x1234, nil", v, err)
	}
	if v, err := d.Uint32(); err != nil || v != 0x12345678 {
		t.Fatalf("Uint32 = %#x, %v; want 0x12345678, nil", v, err)
	}
	if d.Remaining() != 0 {
		t.Errorf("Remaining = %d; want 0", d.Remaining())
	}
}

func TestDecoder_BigEndianPrimitives(t *testing.T) {
	// uint16 at 0, then two pad bytes so the uint32 lands on offset 4.
	d, err := NewDecoder(be(0x12, 0x34, 0x00, 0x00, 0x12, 0x34, 0x56, 0x78))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.Uint16(); err != nil || v != 0x1234 {
		t.Fatalf("Uint16 = %#x, %v; want 0x1234, nil", v, err)
	}
	if v, err := d.Uint32(); err != nil || v != 0x12345678 {
		t.Fatalf("Uint32 = %#x, %v; want 0x12345678, nil", v, err)
	}
}

func TestDecoder_Float32(t *testing.T) {
	// 1.5f == 0x3fc00000
	d, err := NewDecoder(le(0x00, 0x00, 0xc0, 0x3f))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.Float32(); err != nil || v != 1.5 {
		t.Fatalf("Float32 = %v, %v; want 1.5, nil", v, err)
	}
}

func TestDecoder_SignedPrimitives(t *testing.T) {
	d, err := NewDecoder(le(0xff, 0x00, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.Int8(); err != nil || v != -1 {
		t.Fatalf("Int8 = %v, %v; want -1, nil", v, err)
	}
	if v, err := d.Int16(); err != nil || v != -2 {
		t.Fatalf("Int16 = %v, %v; want -2, nil", v, err)
	}
	if v, err := d.Int32(); err != nil || v != -1 {
		t.Fatalf("Int32 = %v, %v; want -1, nil", v, err)
	}
}

func TestDecoder_Bool(t *testing.T) {
	d, err := NewDecoder(le(0x01, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.Bool(); err != nil || !v {
		t.Fatalf("Bool = %v, %v; want true, nil", v, err)
	}
	if v, err := d.Bool(); err != nil || v {
		t.Fatalf("Bool = %v, %v; want false, nil", v, err)
	}
}

func TestDecoder_ShortReadIsErrShort(t *testing.T) {
	d, err := NewDecoder(le(0x01))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Uint32(); !errors.Is(err, ErrShort) {
		t.Fatalf("err = %v; want ErrShort", err)
	}
}

func TestDecoder_String(t *testing.T) {
	// len=6 ("hello\0"), then the bytes.
	d, err := NewDecoder(le(
		0x06, 0x00, 0x00, 0x00,
		'h', 'e', 'l', 'l', 'o', 0x00,
	))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.String(); err != nil || v != "hello" {
		t.Fatalf("String = %q, %v; want \"hello\", nil", v, err)
	}
	if d.Remaining() != 0 {
		t.Errorf("Remaining = %d; want 0", d.Remaining())
	}
}

func TestDecoder_StringEmpty(t *testing.T) {
	d, err := NewDecoder(le(0x01, 0x00, 0x00, 0x00, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.String(); err != nil || v != "" {
		t.Fatalf("String = %q, %v; want \"\", nil", v, err)
	}
}

func TestDecoder_StringTruncated(t *testing.T) {
	d, err := NewDecoder(le(0xff, 0x00, 0x00, 0x00, 'a'))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.String(); !errors.Is(err, ErrShort) {
		t.Fatalf("err = %v; want ErrShort", err)
	}
}

func TestDecoder_SkipFloat32Seq(t *testing.T) {
	// count=2, then two float32s, then a trailing uint8 we expect to reach.
	d, err := NewDecoder(le(
		0x02, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x80, 0x3f, // 1.0
		0x00, 0x00, 0x00, 0x40, // 2.0
		0x7b,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SkipFloat32Seq(); err != nil {
		t.Fatal(err)
	}
	if v, err := d.Uint8(); err != nil || v != 123 {
		t.Fatalf("Uint8 = %v, %v; want 123, nil", v, err)
	}
}

func TestDecoder_SkipFloat32SeqTruncated(t *testing.T) {
	d, err := NewDecoder(le(0x09, 0x00, 0x00, 0x00, 0x00, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SkipFloat32Seq(); !errors.Is(err, ErrShort) {
		t.Fatalf("err = %v; want ErrShort", err)
	}
}

func TestDecoder_SkipBytesPaysAlignment(t *testing.T) {
	// uint8, then a float32[2] that must align to 4 first.
	d, err := NewDecoder(le(
		0x01,
		0x00, 0x00, 0x00, // padding to align 4
		0xde, 0xad, 0xbe, 0xef,
		0xde, 0xad, 0xbe, 0xef,
		0x2a,
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Uint8(); err != nil {
		t.Fatal(err)
	}
	if err := d.SkipBytes(4, 8); err != nil {
		t.Fatal(err)
	}
	if v, err := d.Uint8(); err != nil || v != 42 {
		t.Fatalf("Uint8 = %v, %v; want 42, nil", v, err)
	}
}

func TestDecoder_Float64(t *testing.T) {
	// 1.5 as a little-endian double, after the CDR_LE encapsulation header.
	payload := append([]byte{0x00, 0x01, 0x00, 0x00},
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf8, 0x3f)
	d, err := NewDecoder(payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Float64()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.5 {
		t.Errorf("Float64() = %v, want 1.5", got)
	}
	if d.Remaining() != 0 {
		t.Errorf("Remaining() = %d, want 0", d.Remaining())
	}
}

func TestDecoder_Float64ShortReadIsErrShort(t *testing.T) {
	d, err := NewDecoder([]byte{0x00, 0x01, 0x00, 0x00, 0x01, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Float64(); !errors.Is(err, ErrShort) {
		t.Errorf("err = %v, want ErrShort", err)
	}
}

func TestDecoder_SkipFloat64Seq(t *testing.T) {
	// A five-element distortion vector, then a sentinel the decoder must land on.
	// The doubles align to 8, so four bytes of padding follow the length prefix;
	// getting that wrong is exactly what this test catches.
	payload := []byte{0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00}
	payload = append(payload, make([]byte, 4)...)
	payload = append(payload, make([]byte, 5*8)...)
	payload = append(payload, 0x2a, 0x00, 0x00, 0x00)
	d, err := NewDecoder(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SkipFloat64Seq(); err != nil {
		t.Fatal(err)
	}
	got, err := d.Uint32()
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("sentinel = %d, want 42; the skip misaligned", got)
	}
}

func TestDecoder_SkipFloat64SeqEmpty(t *testing.T) {
	d, err := NewDecoder([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SkipFloat64Seq(); err != nil {
		t.Errorf("an empty sequence should skip cleanly: %v", err)
	}
}
