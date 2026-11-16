package cdr

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzDecoder(f *testing.F) {
	f.Add([]byte{0, 1, 0, 0, 0xff, 0xff, 0xff, 0xff}, []byte{6})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 2, 'a', 0}, []byte{5})
	f.Add([]byte{0, 3, 0, 0, 1, 2, 3}, []byte{0, 4})
	f.Fuzz(func(t *testing.T, payload, operations []byte) {
		if len(payload) > 64<<10 || len(operations) > 128 {
			return
		}
		d, err := NewDecoder(payload)
		if err != nil {
			return
		}
		for _, op := range operations {
			before := d.Remaining()
			switch op % 9 {
			case 0:
				_, err = d.Uint8()
			case 1:
				_, err = d.Uint16()
			case 2:
				_, err = d.Uint32()
			case 3:
				_, err = d.Uint64()
			case 4:
				_, err = d.Float32()
			case 5:
				_, err = d.String()
			case 6:
				_, err = d.Bytes()
			case 7:
				err = d.SkipFloat32Seq()
			case 8:
				err = d.SkipBytes(1<<((op/9)%4), int(op))
			}
			if err != nil {
				return // Callers stop decoding a malformed payload at its first error.
			}
			if d.Remaining() < 0 || d.Remaining() > before {
				t.Fatalf("successful read escaped input: before=%d after=%d", before, d.Remaining())
			}
		}
	})
}

// A fixed wire layout independently exercises byte order and alignment relative
// to the CDR body, including a variable string followed by another sequence.
func FuzzDecoderRoundTrip(f *testing.F) {
	f.Add(uint64(0x0123456789abcdef), "map", []byte{1, 2, 3}, false)
	f.Add(uint64(0), "", []byte{}, true)
	f.Fuzz(func(t *testing.T, value uint64, name string, data []byte, little bool) {
		if len(name) > 4096 || len(data) > 4096 {
			return
		}
		var order binary.ByteOrder = binary.BigEndian
		id := byte(0)
		if little {
			order, id = binary.LittleEndian, 1
		}
		body := make([]byte, 20)
		body[0] = byte(value)
		order.PutUint16(body[2:4], uint16(value))
		order.PutUint32(body[4:8], uint32(value))
		order.PutUint64(body[8:16], value)
		order.PutUint32(body[16:20], uint32(len(name)+1))
		body = append(append(body, []byte(name)...), 0)
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
		countOffset := len(body)
		body = append(body, make([]byte, 4)...)
		order.PutUint32(body[countOffset:], uint32(len(data)))
		body = append(body, data...)
		d, err := NewDecoder(append([]byte{0, id, 0, 0}, body...))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := d.Uint8(); err != nil || got != byte(value) {
			t.Fatalf("uint8: %d %v", got, err)
		}
		if got, err := d.Uint16(); err != nil || got != uint16(value) {
			t.Fatalf("uint16: %d %v", got, err)
		}
		if got, err := d.Uint32(); err != nil || got != uint32(value) {
			t.Fatalf("uint32: %d %v", got, err)
		}
		if got, err := d.Uint64(); err != nil || got != value {
			t.Fatalf("uint64: %d %v", got, err)
		}
		if got, err := d.String(); err != nil || got != name {
			t.Fatalf("string: %q %v", got, err)
		}
		if got, err := d.Bytes(); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("bytes: %x %v", got, err)
		}
		if d.Remaining() != 0 {
			t.Fatalf("unread bytes: %d", d.Remaining())
		}
	})
}
