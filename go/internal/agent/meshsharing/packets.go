package meshsharing

import (
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
)

// PacketAddresses checks wire lengths before any packet is admitted to a lease.
func PacketAddresses(p []byte) (source, destination netip.Addr, ok bool) {
	if len(p) >= 20 && p[0]>>4 == 4 && int(p[0]&15)*4 >= 20 && int(p[0]&15)*4 <= len(p) && int(binary.BigEndian.Uint16(p[2:4])) == len(p) {
		return netip.AddrFrom4([4]byte(p[12:16])), netip.AddrFrom4([4]byte(p[16:20])), true
	}
	if len(p) >= 40 && p[0]>>4 == 6 && int(binary.BigEndian.Uint16(p[4:6]))+40 == len(p) {
		return netip.AddrFrom16([16]byte(p[8:24])), netip.AddrFrom16([16]byte(p[24:40])), true
	}
	return source, destination, false
}

// Frames preserve datagram boundaries across the private broker/extension pipe.
func WriteFrame(w io.Writer, data []byte) error {
	if len(data) == 0 || len(data) > 65535 {
		return errors.New("invalid internet frame size")
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(data)))
	if err := writeControl(w, header[:]); err != nil {
		return err
	}
	return writeControl(w, data)
}
func ReadFrame(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint16(header[:])
	if size == 0 {
		return nil, errors.New("empty internet frame")
	}
	data := make([]byte, int(size))
	_, err := io.ReadFull(r, data)
	return data, err
}
