package localmesh

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// TCPPacketConn preserves packet boundaries for the explicitly configured
// topology simulator. QUIC still authenticates and encrypts each link. TCP's
// head-of-line blocking makes this unsuitable as a radio or production carrier.
type TCPPacketConn struct {
	conn          net.Conn
	write         sync.Mutex
	local, remote *net.UDPAddr
}

func NewTCPPacketConn(conn net.Conn) (*TCPPacketConn, error) {
	local, lok := conn.LocalAddr().(*net.TCPAddr)
	remote, rok := conn.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok {
		return nil, errors.New("TCP packet carrier requires TCP addresses")
	}
	return &TCPPacketConn{conn: conn,
		local:  &net.UDPAddr{IP: local.IP, Port: local.Port, Zone: local.Zone},
		remote: &net.UDPAddr{IP: remote.IP, Port: remote.Port, Zone: remote.Zone}}, nil
}

func (p *TCPPacketConn) ReadFrom(buf []byte) (int, net.Addr, error) {
	var header [2]byte
	if _, err := io.ReadFull(p.conn, header[:]); err != nil {
		return 0, nil, err
	}
	size := int(binary.BigEndian.Uint16(header[:]))
	if size == 0 || size > 4096 || size > len(buf) {
		return 0, nil, errors.New("invalid framed QUIC packet length")
	}
	n, err := io.ReadFull(p.conn, buf[:size])
	return n, p.remote, err
}

func (p *TCPPacketConn) WriteTo(buf []byte, addr net.Addr) (int, error) {
	remote, ok := addr.(*net.UDPAddr)
	if !ok || !remote.IP.Equal(p.remote.IP) || remote.Port != p.remote.Port || remote.Zone != p.remote.Zone {
		return 0, errors.New("framed QUIC packet has unknown destination")
	}
	if len(buf) == 0 || len(buf) > 4096 {
		return 0, errors.New("invalid framed QUIC packet length")
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(buf)))
	p.write.Lock()
	defer p.write.Unlock()
	if err := writeFull(p.conn, header[:]); err != nil {
		return 0, err
	}
	if err := writeFull(p.conn, buf); err != nil {
		return 0, err
	}
	return len(buf), nil
}

func writeFull(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (p *TCPPacketConn) Close() error                       { return p.conn.Close() }
func (p *TCPPacketConn) LocalAddr() net.Addr                { return p.local }
func (p *TCPPacketConn) RemoteAddr() net.Addr               { return p.remote }
func (p *TCPPacketConn) SetDeadline(t time.Time) error      { return p.conn.SetDeadline(t) }
func (p *TCPPacketConn) SetReadDeadline(t time.Time) error  { return p.conn.SetReadDeadline(t) }
func (p *TCPPacketConn) SetWriteDeadline(t time.Time) error { return p.conn.SetWriteDeadline(t) }
