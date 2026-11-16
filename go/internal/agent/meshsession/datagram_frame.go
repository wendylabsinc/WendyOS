package meshsession

import (
	"encoding/binary"
	"io"
	"syscall"
)

// tlsDatagramReader retains partial frames across read deadlines. The caller
// owns serialization, deadlines, storage accounting and connection lifetime.
// Returned bytes remain valid until the next call.
type tlsDatagramReader struct {
	header     [2]byte
	headerUsed int
	payload    []byte
	used       int
}

func (r *tlsDatagramReader) read(conn io.Reader, maximum int) ([]byte, bool, error) {
	if r.headerUsed < len(r.header) {
		n, err := conn.Read(r.header[r.headerUsed:])
		r.headerUsed += n
		if r.headerUsed < len(r.header) {
			return nil, false, frameReadError(n, err)
		}
	}
	size := int(binary.BigEndian.Uint16(r.header[:]))
	if size > maximum {
		return nil, false, syscall.EMSGSIZE
	}
	if cap(r.payload) < size {
		r.payload = make([]byte, size)
	}
	r.payload = r.payload[:size]
	if r.used < size {
		n, err := conn.Read(r.payload[r.used:])
		r.used += n
		if r.used < size {
			return nil, false, frameReadError(n, err)
		}
	}
	r.headerUsed, r.used = 0, 0
	return r.payload, true, nil
}

func frameReadError(n int, err error) error {
	if n == 0 && err == nil {
		return io.ErrNoProgress
	}
	return err
}

func writeTLSDatagramFrame(conn io.Writer, payload []byte) error {
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
	for _, part := range [][]byte{header[:], payload} {
		for len(part) != 0 {
			n, err := conn.Write(part)
			if err != nil {
				return err
			}
			if n <= 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}
