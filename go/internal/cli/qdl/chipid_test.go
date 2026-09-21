package qdl

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// failNthWriteConn drops the Nth host write, not the device's reply to it.
type failNthWriteConn struct {
	*fakeConn
	failAt int
	writes int
}

func (c *failNthWriteConn) Write(p []byte, timeout time.Duration) (int, error) {
	c.writes++
	if c.writes == c.failAt {
		return 0, errors.New("write failed")
	}
	return c.fakeConn.Write(p, timeout)
}

// wantSwitchModeLast asserts the session was handed to image transfer instead
// of reset. Short writes are reported rather than indexed, which would panic
// and take the whole test binary down.
func wantSwitchModeLast(t *testing.T, writes [][]byte) {
	t.Helper()
	if len(writes) == 0 {
		t.Fatal("host wrote nothing")
	}
	last := writes[len(writes)-1]
	if len(last) < saharaCmdSwitchModeLen {
		t.Fatalf("last write is %d bytes, too short for a SWITCH_MODE", len(last))
	}
	if cmd := binary.LittleEndian.Uint32(last[0:4]); cmd != saharaCmdSwitchMode {
		t.Fatalf("last write cmd = 0x%x, want SWITCH_MODE (0x%x)", cmd, saharaCmdSwitchMode)
	}
	if n := binary.LittleEndian.Uint32(last[4:8]); int(n) != saharaCmdSwitchModeLen {
		t.Errorf("SWITCH_MODE declares %d bytes, want %d", n, saharaCmdSwitchModeLen)
	}
	if mode := binary.LittleEndian.Uint32(last[8:12]); mode != saharaModeImageTransfer {
		t.Errorf("SWITCH_MODE mode = %d, want image transfer (%d)", mode, saharaModeImageTransfer)
	}
}

func TestReadChipIDReturnsTheMsmID(t *testing.T) {
	hwid := make([]byte, 8)
	binary.LittleEndian.PutUint64(hwid, 0x002eb0e100000000)

	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen),
		pkt(saharaCmdReady, saharaCmdReadyLen),
		pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, 8),
		hwid,
	}}

	got, err := ReadChipID(c)
	if err != nil {
		t.Fatalf("ReadChipID: %v", err)
	}
	if got.MsmID != 0x002eb0e1 {
		t.Errorf("MsmID = %#x, want 0x002eb0e1", got.MsmID)
	}
	if len(c.writes) == 0 {
		t.Fatal("host wrote nothing")
	}
	if len(c.writes[0]) < 24 {
		t.Fatalf("HELLO_RESP is %d bytes, too short to carry the mode", len(c.writes[0]))
	}
	// Echoing the device's image-transfer mode would never yield CMD_READY.
	if mode := binary.LittleEndian.Uint32(c.writes[0][20:24]); mode != saharaModeCommand {
		t.Errorf("HELLO_RESP mode = %d, want %d", mode, saharaModeCommand)
	}
}

func TestReadChipIDFailsWhenTheDeviceGoesQuiet(t *testing.T) {
	if _, err := ReadChipID(&fakeConn{}); err == nil {
		t.Error("ReadChipID succeeded against a silent device")
	}
}

// A zero chip id would match no board, so an unexpected reply must error.
func TestReadChipIDRejectsAnUnexpectedReply(t *testing.T) {
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen),
		pkt(saharaReadData, saharaReadDataLen),
	}}
	if _, err := ReadChipID(c); err == nil {
		t.Error("ReadChipID accepted a READ_DATA where CMD_READY was required")
	}
}

// An 8-byte RESET_RESP landing where the raw id belongs decodes into a
// plausible chip id, which the install would then blame on the hardware.
func TestReadChipIDRejectsASaharaPacketWhereTheIDBelongs(t *testing.T) {
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen),
		pkt(saharaCmdReady, saharaCmdReadyLen),
		pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, saharaMsmHWIDLen),
		pkt(saharaResetResp, 8),
	}}
	if _, err := ReadChipID(c); err == nil {
		t.Error("ReadChipID decoded a RESET_RESP as a chip id")
	}
}

// Lemans silicon declares a 24-byte MSM_HWID payload, not the 8 bytes the id
// itself occupies, and the whole declared payload has to be read.
func TestReadChipIDReadsAPayloadLongerThanTheID(t *testing.T) {
	payload := make([]byte, 24)
	binary.LittleEndian.PutUint64(payload, 0x002eb0e100000000)

	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen),
		pkt(saharaCmdReady, saharaCmdReadyLen),
		pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, 24),
		payload,
	}}

	got, err := ReadChipID(c)
	if err != nil {
		t.Fatalf("ReadChipID: %v", err)
	}
	if got.MsmID != 0x002eb0e1 {
		t.Errorf("MsmID = %#x, want 0x002eb0e1", got.MsmID)
	}
}

// A declared payload split across bulk transfers must be reassembled, not read
// as a short one.
func TestReadChipIDReassemblesASplitPayload(t *testing.T) {
	payload := make([]byte, 24)
	binary.LittleEndian.PutUint64(payload, 0x002e70e100000000)

	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen),
		pkt(saharaCmdReady, saharaCmdReadyLen),
		pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, 24),
		payload[:5],
		payload[5:],
	}}

	got, err := ReadChipID(c)
	if err != nil {
		t.Fatalf("ReadChipID: %v", err)
	}
	if got.MsmID != 0x002e70e1 {
		t.Errorf("MsmID = %#x, want 0x002e70e1", got.MsmID)
	}
}

// CMD_EXEC_RESP names the exec it answers and the payload size that follows,
// so a reply describing anything else must not be decoded.
func TestReadChipIDRejectsAMismatchedExecResponse(t *testing.T) {
	for name, resp := range map[string][]byte{
		"another client command":             pkt(saharaCmdExecResp, saharaCmdExecRespLen, 0x07, saharaMsmHWIDLen),
		"a response too short to hold an id": pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, 4),
		"an implausible response length":     pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			hwid := make([]byte, 8)
			binary.LittleEndian.PutUint64(hwid, 0x002eb0e100000000)
			c := &fakeConn{reads: [][]byte{
				pkt(saharaHello, saharaHelloLen),
				pkt(saharaCmdReady, saharaCmdReadyLen),
				resp,
				hwid,
			}}
			if _, err := ReadChipID(c); err == nil {
				t.Error("ReadChipID accepted an exec response for something else")
			}
		})
	}
}

// A desync must still switch the session to image transfer: a RESET reboots the
// board out of EDL, and leaving it in command mode makes UploadProgrammer read
// the silence as "already in Firehose mode" and skip flashing it.
func TestReadChipIDSwitchesToImageTransferOnDesync(t *testing.T) {
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen),
		pkt(saharaReadData, saharaReadDataLen),
	}}
	if _, err := ReadChipID(c); err == nil {
		t.Fatal("want an error for an unexpected reply")
	}
	wantSwitchModeLast(t, c.writes)
}

// The flash continues on this connection whatever the read did, so the mode
// switch is what every outcome has to end with.
func TestReadChipIDEndsInImageTransferMode(t *testing.T) {
	hwid := make([]byte, 8)
	binary.LittleEndian.PutUint64(hwid, 0x002eb0e100000000)

	for name, tc := range map[string]struct {
		reads   [][]byte
		wantErr bool
	}{
		"a successful read": {
			reads: [][]byte{
				pkt(saharaHello, saharaHelloLen),
				pkt(saharaCmdReady, saharaCmdReadyLen),
				pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, saharaMsmHWIDLen),
				hwid,
			},
		},
		"a read that fails after HELLO_RESP": {
			reads: [][]byte{
				pkt(saharaHello, saharaHelloLen),
				pkt(saharaCmdReady, saharaCmdReadyLen),
				pkt(saharaCmdExecResp, saharaCmdExecRespLen, 0x07, saharaMsmHWIDLen),
			},
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeConn{reads: tc.reads}
			_, err := ReadChipID(c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReadChipID error = %v, want an error: %v", err, tc.wantErr)
			}
			wantSwitchModeLast(t, c.writes)
		})
	}
}

// A dropped write must surface as an error and still hand the session over.
func TestReadChipIDFailsWhenAWriteFails(t *testing.T) {
	for name, tc := range map[string]struct {
		failAt int
		reads  [][]byte
	}{
		"hello resp": {
			failAt: 1,
			reads:  [][]byte{pkt(saharaHello, saharaHelloLen)},
		},
		"cmd exec": {
			failAt: 2,
			reads: [][]byte{
				pkt(saharaHello, saharaHelloLen),
				pkt(saharaCmdReady, saharaCmdReadyLen),
			},
		},
		"cmd exec data": {
			failAt: 3,
			reads: [][]byte{
				pkt(saharaHello, saharaHelloLen),
				pkt(saharaCmdReady, saharaCmdReadyLen),
				pkt(saharaCmdExecResp, saharaCmdExecRespLen, saharaExecMsmHWIDRead, 8),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &failNthWriteConn{
				fakeConn: &fakeConn{reads: tc.reads},
				failAt:   tc.failAt,
			}
			if _, err := ReadChipID(c); err == nil {
				t.Fatal("ReadChipID succeeded despite a write failing")
			}
			wantSwitchModeLast(t, c.fakeConn.writes)
		})
	}
}
