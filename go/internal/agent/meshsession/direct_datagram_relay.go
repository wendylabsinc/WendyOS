package meshsession

import (
	"io"
	"net"
)

// A read consumes exactly one message. The extra byte detects oversized local
// datagrams without forwarding their truncated prefix. Empty messages must
// reach Write; io.Copy's stream semantics are unsuitable here.
func relayDirectDatagrams(dst, src net.Conn, maximum int) error {
	buffer := make([]byte, maximum+1)
	for {
		n, err := src.Read(buffer)
		if err != nil {
			return err
		}
		if n > maximum {
			continue
		}
		written, err := dst.Write(buffer[:n])
		if err != nil {
			return err
		}
		if written != n {
			return io.ErrShortWrite
		}
	}
}
