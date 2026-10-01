// Package cdr decodes classic CDR (XCDR1) payloads, the wire format ROS 2
// uses for user data on DDS. It is decode-only and covers the subset used by
// Wendy's ROS 2 telemetry and camera messages.
package cdr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrShort reports a payload that ended before the requested field.
var ErrShort = errors.New("cdr: payload too short")

// Decoder reads a CDR body. Alignment is measured from the start of the body,
// i.e. excluding the 4-byte encapsulation header, which is what the RTPS spec
// requires and what both CycloneDDS and Fast DDS emit.
type Decoder struct {
	buf   []byte
	pos   int
	order binary.ByteOrder
}

// NewDecoder splits the encapsulation header off payload and selects the byte
// order it names. The identifier itself is always big-endian.
func NewDecoder(payload []byte) (*Decoder, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("reading encapsulation header: %w", ErrShort)
	}
	var order binary.ByteOrder
	switch id := binary.BigEndian.Uint16(payload[0:2]); id {
	case 0x0000, 0x0002: // CDR_BE, PL_CDR_BE
		order = binary.BigEndian
	case 0x0001, 0x0003: // CDR_LE, PL_CDR_LE
		order = binary.LittleEndian
	default:
		return nil, fmt.Errorf("cdr: unsupported representation identifier %#04x", id)
	}
	return &Decoder{buf: payload[4:], order: order}, nil
}

// Remaining reports the unread byte count, used to assert that a decoder
// consumed a payload exactly.
func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

// align advances pos to the next multiple of n.
func (d *Decoder) align(n int) {
	if r := d.pos % n; r != 0 {
		d.pos += n - r
	}
}

// take aligns to n, then consumes n bytes.
func (d *Decoder) take(n int) ([]byte, error) {
	d.align(n)
	if d.pos+n > len(d.buf) {
		return nil, ErrShort
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

// Uint8 reads a single byte.
func (d *Decoder) Uint8() (uint8, error) {
	b, err := d.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// Int8 reads a signed byte.
func (d *Decoder) Int8() (int8, error) {
	v, err := d.Uint8()
	return int8(v), err
}

// Bool reads a CDR boolean, which occupies one byte.
func (d *Decoder) Bool() (bool, error) {
	v, err := d.Uint8()
	return v != 0, err
}

// Uint16 reads a 2-byte unsigned integer, aligned to 2.
func (d *Decoder) Uint16() (uint16, error) {
	b, err := d.take(2)
	if err != nil {
		return 0, err
	}
	return d.order.Uint16(b), nil
}

// Int16 reads a 2-byte signed integer, aligned to 2.
func (d *Decoder) Int16() (int16, error) {
	v, err := d.Uint16()
	return int16(v), err
}

// Uint32 reads a 4-byte unsigned integer, aligned to 4.
func (d *Decoder) Uint32() (uint32, error) {
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return d.order.Uint32(b), nil
}

// Int32 reads a 4-byte signed integer, aligned to 4.
func (d *Decoder) Int32() (int32, error) {
	v, err := d.Uint32()
	return int32(v), err
}

// Uint64 reads an 8-byte unsigned integer, aligned to 8.
func (d *Decoder) Uint64() (uint64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return d.order.Uint64(b), nil
}

// Float32 reads an IEEE-754 single, aligned to 4.
func (d *Decoder) Float32() (float32, error) {
	v, err := d.Uint32()
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(v), nil
}

// String reads a CDR string: a uint32 length that includes the NUL
// terminator, then that many bytes.
func (d *Decoder) String() (string, error) {
	n, err := d.Uint32()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	if int(n) > len(d.buf)-d.pos {
		return "", ErrShort
	}
	s := d.buf[d.pos : d.pos+int(n)]
	d.pos += int(n)
	if len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	return string(s), nil
}

// SkipString steps over a string without allocating it.
func (d *Decoder) SkipString() error {
	_, err := d.String()
	return err
}

// Bytes reads a sequence<uint8>: a uint32 element count followed by the bytes.
// The returned slice aliases the serialized payload and is valid for as long as
// the Decoder's input remains live.
func (d *Decoder) Bytes() ([]byte, error) {
	n, err := d.Uint32()
	if err != nil {
		return nil, err
	}
	if uint64(n) > uint64(len(d.buf)-d.pos) {
		return nil, ErrShort
	}
	b := d.buf[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return b, nil
}

// SkipBytes aligns to align, then steps over n bytes. Use it for fixed-size
// arrays, whose alignment is that of their element type.
func (d *Decoder) SkipBytes(align, n int) error {
	d.align(align)
	if d.pos+n > len(d.buf) {
		return ErrShort
	}
	d.pos += n
	return nil
}

// Float64 reads an IEEE-754 double. Camera intrinsics are float64, so a decoder
// that stopped at Float32 could not read a CameraInfo projection matrix.
func (d *Decoder) Float64() (float64, error) {
	v, err := d.Uint64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(v), nil
}

// SkipFloat64Seq skips a length-prefixed sequence of doubles, such as a
// CameraInfo distortion vector whose length varies by model.
func (d *Decoder) SkipFloat64Seq() error {
	n, err := d.Uint32()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	return d.SkipBytes(8, int(n)*8)
}

// SkipFloat32Seq steps over a sequence<float32>: a uint32 count, then that
// many 4-byte elements.
func (d *Decoder) SkipFloat32Seq() error {
	n, err := d.Uint32()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	return d.SkipBytes(4, int(n)*4)
}
