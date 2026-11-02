package meshsession

import (
	"crypto/tls"
	"io"
	"sync"
)

// Plain TLS app sessions serve constrained peers. Bound each application
// record's temporary receive/decrypt workspace without limiting stream or
// datagram length. This does not alter TLS handshake records or receive limits.
const tlsAppRecordPayload = 1024

type tlsRecordConn struct {
	*tls.Conn
	writeMu sync.Mutex
}

func (c *tlsRecordConn) Write(payload []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(payload) == 0 {
		return c.Conn.Write(payload)
	}
	written := 0
	for len(payload) > 0 {
		chunk := payload[:min(tlsAppRecordPayload, len(payload))]
		n, err := c.Conn.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		if n != len(chunk) {
			return written, io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return written, nil
}

func (c *tlsRecordConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.Conn.CloseWrite()
}
