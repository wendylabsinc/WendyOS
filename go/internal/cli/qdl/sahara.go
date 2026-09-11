package qdl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Sahara command ids. Only the subset needed to hand the device its Firehose
// programmer is implemented; memory-debug and command mode are not.
const (
	saharaHello      uint32 = 0x01
	saharaHelloResp  uint32 = 0x02
	saharaReadData   uint32 = 0x03
	saharaEndOfImage uint32 = 0x04
	saharaDone       uint32 = 0x05
	saharaDoneResp   uint32 = 0x06
	saharaReset      uint32 = 0x07
	saharaResetResp  uint32 = 0x08
	saharaReadData64 uint32 = 0x12
)

// Packet lengths, which the device also uses to validate our replies.
const (
	saharaHelloLen      = 0x30
	saharaReadDataLen   = 0x14
	saharaReadData64Len = 0x20
	saharaEndOfImageLen = 0x10
	saharaDoneLen       = 0x08
	saharaDoneRespLen   = 0x0c
	saharaResetLen      = 0x08
)

// saharaVersion is the protocol version we advertise; 2 is what every EDL
// target in this family speaks.
const saharaVersion = 2

// programmerImageID is the image id a device asks for when it wants a plain
// (non-archive) Firehose programmer ELF.
const programmerImageID = 13

// saharaTimeout bounds a single command exchange.
const saharaTimeout = time.Second

// ErrTimeout reports that a transport read produced nothing in time. Sahara
// treats it as a signal rather than a failure: a silent device is how an
// already-running Firehose programmer presents itself.
var ErrTimeout = errors.New("qdl: timed out waiting for the device")

// UploadProgrammer runs the Sahara handshake, serving prog until the device
// reports the image complete. Returns nil without doing anything if a Firehose
// programmer is already running, as a previous flash may have left one up.
func UploadProgrammer(conn Conn, name string, prog []byte, progress func(sent, total int64)) error {
	buf := make([]byte, 4096)
	for first := true; ; first = false {
		n, err := conn.Read(buf, saharaTimeout)
		if err != nil {
			// A device already in Firehose mode never sends HELLO.
			if first && errors.Is(err, ErrTimeout) {
				return nil
			}
			return fmt.Errorf("reading sahara request: %w", err)
		}
		// It may instead answer our probe with an XML parse complaint.
		if first && n >= 5 && string(buf[:5]) == "<?xml" {
			return nil
		}

		cmd, length, err := saharaHeader(buf[:n])
		if err != nil {
			sendSaharaReset(conn)
			return err
		}

		switch cmd {
		case saharaHello:
			if length != saharaHelloLen {
				sendSaharaReset(conn)
				return fmt.Errorf("sahara HELLO has length %d, want %d", length, saharaHelloLen)
			}
			// Echo the mode the device asked for; picking our own would
			// put the two ends into different modes.
			mode := binary.LittleEndian.Uint32(buf[20:24])
			if err := sendSaharaHelloResp(conn, mode); err != nil {
				return err
			}
		case saharaReadData, saharaReadData64:
			if err := serveSaharaRead(conn, cmd, buf[:n], name, prog, progress); err != nil {
				sendSaharaReset(conn)
				return err
			}
		case saharaEndOfImage:
			if length != saharaEndOfImageLen {
				sendSaharaReset(conn)
				return fmt.Errorf("sahara END_OF_IMAGE has length %d, want %d", length, saharaEndOfImageLen)
			}
			if status := binary.LittleEndian.Uint32(buf[12:16]); status != 0 {
				return fmt.Errorf("device rejected the programmer image (status %d)", status)
			}
			if err := writePacket(conn, saharaDone, saharaDoneLen, nil); err != nil {
				return err
			}
		case saharaDoneResp:
			if length != saharaDoneRespLen {
				sendSaharaReset(conn)
				return fmt.Errorf("sahara DONE_RESP has length %d, want %d", length, saharaDoneRespLen)
			}
			// 0 means the device wants another image; we only ever have one,
			// so anything but "complete" is a mismatch we cannot satisfy.
			if binary.LittleEndian.Uint32(buf[8:12]) == 1 {
				return nil
			}
			return errors.New("device asked for more Sahara images than the bundle provides")
		case saharaResetResp:
			return errors.New("device reset the sahara session")
		default:
			// Unknown commands are not fatal on their own, but we have
			// nothing useful to reply with, so fail loudly rather than
			// spin until the caller's deadline.
			return fmt.Errorf("unexpected sahara command 0x%x", cmd)
		}
	}
}

// serveSaharaRead answers a READ_DATA or READ_DATA64 request with the slice of
// the programmer the device asked for.
func serveSaharaRead(conn Conn, cmd uint32, pkt []byte, name string, prog []byte, progress func(sent, total int64)) error {
	var image, offset, length uint64
	switch cmd {
	case saharaReadData:
		if len(pkt) != saharaReadDataLen {
			return fmt.Errorf("sahara READ_DATA has length %d, want %d", len(pkt), saharaReadDataLen)
		}
		image = uint64(binary.LittleEndian.Uint32(pkt[8:12]))
		offset = uint64(binary.LittleEndian.Uint32(pkt[12:16]))
		length = uint64(binary.LittleEndian.Uint32(pkt[16:20]))
	default:
		if len(pkt) != saharaReadData64Len {
			return fmt.Errorf("sahara READ_DATA64 has length %d, want %d", len(pkt), saharaReadData64Len)
		}
		image = binary.LittleEndian.Uint64(pkt[8:16])
		offset = binary.LittleEndian.Uint64(pkt[16:24])
		length = binary.LittleEndian.Uint64(pkt[24:32])
	}

	if image != programmerImageID {
		return fmt.Errorf("device requested sahara image %d, but the bundle only provides the programmer (%d)",
			image, programmerImageID)
	}
	total := uint64(len(prog))
	if offset > total || length > total-offset {
		return fmt.Errorf("device requested bytes %d..%d of a %d-byte programmer", offset, offset+length, total)
	}

	if _, err := conn.Write(prog[offset:offset+length], saharaTimeout); err != nil {
		return fmt.Errorf("sending %s: %w", name, err)
	}
	if progress != nil {
		progress(int64(offset+length), int64(total))
	}
	return nil
}

func sendSaharaHelloResp(conn Conn, mode uint32) error {
	// version, compatible, status, mode, then six reserved words.
	payload := make([]byte, saharaHelloLen-8)
	binary.LittleEndian.PutUint32(payload[0:4], saharaVersion)
	binary.LittleEndian.PutUint32(payload[4:8], 1)
	binary.LittleEndian.PutUint32(payload[8:12], 0)
	binary.LittleEndian.PutUint32(payload[12:16], mode)
	return writePacket(conn, saharaHelloResp, saharaHelloLen, payload)
}

// sendSaharaReset tells the device to abandon the session. Errors are ignored:
// it is a courtesy on a path that is already failing.
func sendSaharaReset(conn Conn) {
	_ = writePacket(conn, saharaReset, saharaResetLen, nil)
}

func writePacket(conn Conn, cmd uint32, length int, payload []byte) error {
	pkt := make([]byte, length)
	binary.LittleEndian.PutUint32(pkt[0:4], cmd)
	binary.LittleEndian.PutUint32(pkt[4:8], uint32(length))
	copy(pkt[8:], payload)
	if _, err := conn.Write(pkt, saharaTimeout); err != nil {
		return fmt.Errorf("writing sahara command 0x%x: %w", cmd, err)
	}
	return nil
}

// saharaHeader validates the framing shared by every packet: the device states
// its own length, and a mismatch means we are out of sync with the stream.
func saharaHeader(pkt []byte) (cmd uint32, length int, err error) {
	if len(pkt) < 8 {
		return 0, 0, fmt.Errorf("sahara packet is %d bytes, too short for a header", len(pkt))
	}
	cmd = binary.LittleEndian.Uint32(pkt[0:4])
	length = int(binary.LittleEndian.Uint32(pkt[4:8]))
	if length != len(pkt) {
		return 0, 0, fmt.Errorf("sahara packet declares %d bytes but %d arrived", length, len(pkt))
	}
	return cmd, length, nil
}
