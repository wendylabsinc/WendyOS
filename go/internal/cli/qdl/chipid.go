package qdl

import (
	"encoding/binary"
	"fmt"
)

// Sahara command mode, used only to read the chip id. The image-transfer
// subset lives in sahara.go.
const (
	saharaCmdReady      uint32 = 0x0b
	saharaCmdSwitchMode uint32 = 0x0c
	saharaCmdExec       uint32 = 0x0d
	saharaCmdExecResp   uint32 = 0x0e
	saharaCmdExecData   uint32 = 0x0f
)

const (
	saharaCmdReadyLen      = 0x08
	saharaCmdSwitchModeLen = 0x0c
	saharaCmdExecLen       = 0x0c
	saharaCmdExecRespLen   = 0x10
	saharaCmdExecDataLen   = 0x0c
)

// Session modes: the host asks for command mode in HELLO_RESP to reach the
// chip-id read, and switches back to image transfer to hand the session over.
const (
	saharaModeImageTransfer uint32 = 0
	saharaModeCommand       uint32 = 3
)

const saharaExecMsmHWIDRead uint32 = 0x02

// saharaMsmHWIDLen is the part of the MSM_HWID_READ payload that carries the
// 64-bit HW id. Devices declare more than this (24 bytes on Lemans silicon) and
// the rest must still be read, or it becomes the next session's first reply.
const saharaMsmHWIDLen = 8

// saharaMaxExecResponse bounds a declared exec payload, so a desynced length
// cannot make the host wait on bytes that will never arrive.
const saharaMaxExecResponse = 1024

// ChipID identifies the SoC behind the generic 05c6:9008 EDL id.
type ChipID struct {
	MsmID uint32
}

// ReadChipIDFor opens a connection of its own, which only a diagnostic can
// afford: the session this leaves in image-transfer mode belongs to that
// connection, so an install reads the id on the connection it flashes with.
func ReadChipIDFor(dev DeviceInfo) (ChipID, error) {
	conn, err := Open(dev)
	if err != nil {
		return ChipID{}, err
	}
	defer conn.Close() //nolint:errcheck
	return ReadChipID(conn)
}

// ReadChipID runs a command-mode Sahara session to read the chip's MSM_ID. It
// always ends in SWITCH_MODE, never RESET: a RESET reboots the SoC, which then
// stops offering Sahara HELLO until it is power-cycled, while SWITCH_MODE hands
// the live session to image transfer even when the read itself failed.
func ReadChipID(conn Conn) (ChipID, error) {
	id, err := readChipID(conn)
	sendSaharaSwitchMode(conn, saharaModeImageTransfer)
	return id, err
}

// sendSaharaSwitchMode restarts the session in another mode; the device answers
// with a fresh HELLO for it. Errors are ignored: a connection this write cannot
// reach fails the caller's next exchange anyway.
func sendSaharaSwitchMode(conn Conn, mode uint32) {
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, mode)
	_ = writePacket(conn, saharaCmdSwitchMode, saharaCmdSwitchModeLen, payload)
}

func readChipID(conn Conn) (ChipID, error) {
	if _, err := expectSahara(conn, saharaHello, saharaHelloLen); err != nil {
		return ChipID{}, err
	}
	// Request command mode rather than echoing: only that yields CMD_READY.
	if err := sendSaharaHelloResp(conn, saharaModeCommand); err != nil {
		return ChipID{}, err
	}

	if _, err := expectSahara(conn, saharaCmdReady, saharaCmdReadyLen); err != nil {
		return ChipID{}, err
	}

	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, saharaExecMsmHWIDRead)
	if err := writePacket(conn, saharaCmdExec, saharaCmdExecLen, body); err != nil {
		return ChipID{}, err
	}

	resp, err := expectSahara(conn, saharaCmdExecResp, saharaCmdExecRespLen)
	if err != nil {
		return ChipID{}, err
	}
	// The device answers every pending exec on the same command id, so the
	// reply only describes ours if these two fields say so.
	if cmd := binary.LittleEndian.Uint32(resp[8:12]); cmd != saharaExecMsmHWIDRead {
		return ChipID{}, fmt.Errorf("sahara exec response is for client command 0x%x, want 0x%x", cmd, saharaExecMsmHWIDRead)
	}
	respLen := int(binary.LittleEndian.Uint32(resp[12:16]))
	if respLen < saharaMsmHWIDLen || respLen > saharaMaxExecResponse {
		return ChipID{}, fmt.Errorf("sahara exec response declares %d bytes, want %d..%d",
			respLen, saharaMsmHWIDLen, saharaMaxExecResponse)
	}

	if err := writePacket(conn, saharaCmdExecData, saharaCmdExecDataLen, body); err != nil {
		return ChipID{}, err
	}

	buf := make([]byte, 0, respLen)
	for len(buf) < respLen {
		chunk := make([]byte, saharaMaxExecResponse)
		n, err := conn.Read(chunk, saharaTimeout)
		if err != nil {
			return ChipID{}, fmt.Errorf("reading the chip id: %w", err)
		}
		if n == 0 {
			break
		}
		buf = append(buf, chunk[:n]...)
	}
	if len(buf) < respLen {
		return ChipID{}, fmt.Errorf("chip id is %d bytes, want %d", len(buf), respLen)
	}
	// This read carries raw id bytes; anything that frames as a Sahara packet
	// is a desync, and would otherwise decode into a plausible chip id.
	if cmd, _, hdrErr := saharaHeader(buf); hdrErr == nil {
		return ChipID{}, fmt.Errorf("expected the chip id, got sahara command 0x%x", cmd)
	}
	hw := binary.LittleEndian.Uint64(buf[:saharaMsmHWIDLen])
	return ChipID{MsmID: uint32(hw >> 32)}, nil
}

func expectSahara(conn Conn, want uint32, wantLen int) ([]byte, error) {
	buf := make([]byte, 4096)
	n, err := conn.Read(buf, saharaTimeout)
	if err != nil {
		return nil, fmt.Errorf("waiting for sahara command 0x%x: %w", want, err)
	}
	cmd, length, err := saharaHeader(buf[:n])
	if err != nil {
		return nil, err
	}
	if cmd != want {
		return nil, fmt.Errorf("expected sahara command 0x%x, got 0x%x", want, cmd)
	}
	if length != wantLen {
		return nil, fmt.Errorf("sahara command 0x%x has length %d, want %d", cmd, length, wantLen)
	}
	return buf[:n], nil
}
