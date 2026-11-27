package cdr

import (
	"encoding/binary"
	"math"
)

// Encoder writes a little-endian classic CDR (XCDR1) payload, the counterpart
// of Decoder. Alignment is measured from the start of the body, excluding the
// 4-byte encapsulation header.
type Encoder struct {
	buf []byte
}

// NewEncoder starts a payload with the CDR_LE encapsulation header.
func NewEncoder() *Encoder {
	return &Encoder{buf: []byte{0x00, 0x01, 0x00, 0x00}}
}

// Payload returns the serialized payload, including its encapsulation header.
func (e *Encoder) Payload() []byte { return e.buf }

// align pads the body with zeros to the next multiple of n.
func (e *Encoder) align(n int) {
	for (len(e.buf)-4)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

// Uint8 writes a single byte.
func (e *Encoder) Uint8(v uint8) { e.buf = append(e.buf, v) }

// Bool writes a CDR boolean, which occupies one byte.
func (e *Encoder) Bool(v bool) {
	if v {
		e.Uint8(1)
		return
	}
	e.Uint8(0)
}

// Uint16 writes a 2-byte unsigned integer, aligned to 2.
func (e *Encoder) Uint16(v uint16) {
	e.align(2)
	e.buf = binary.LittleEndian.AppendUint16(e.buf, v)
}

// Uint32 writes a 4-byte unsigned integer, aligned to 4.
func (e *Encoder) Uint32(v uint32) {
	e.align(4)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

// Int32 writes a 4-byte signed integer, aligned to 4.
func (e *Encoder) Int32(v int32) { e.Uint32(uint32(v)) }

// Uint64 writes an 8-byte unsigned integer, aligned to 8.
func (e *Encoder) Uint64(v uint64) {
	e.align(8)
	e.buf = binary.LittleEndian.AppendUint64(e.buf, v)
}

// Float32 writes an IEEE-754 single, aligned to 4.
func (e *Encoder) Float32(v float32) { e.Uint32(math.Float32bits(v)) }

// Float64 writes an IEEE-754 double, aligned to 8.
func (e *Encoder) Float64(v float64) { e.Uint64(math.Float64bits(v)) }

// String writes a CDR string: a uint32 length that includes the NUL
// terminator, then the bytes and the terminator. An empty string is one NUL.
func (e *Encoder) String(s string) {
	e.Uint32(uint32(len(s) + 1))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0)
}

// Bytes writes a sequence<uint8>: a uint32 element count followed by the bytes.
func (e *Encoder) Bytes(b []byte) {
	e.Uint32(uint32(len(b)))
	e.buf = append(e.buf, b...)
}
