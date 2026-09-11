package qdl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeConn replays a scripted device side. Sahara is entirely device-driven, so
// reads can be queued up front and the host's replies checked afterwards.
type fakeConn struct {
	reads  [][]byte
	writes [][]byte
	closed bool
}

func (c *fakeConn) Read(p []byte, _ time.Duration) (int, error) {
	if len(c.reads) == 0 {
		return 0, ErrTimeout
	}
	msg := c.reads[0]
	c.reads = c.reads[1:]
	return copy(p, msg), nil
}

func (c *fakeConn) Write(p []byte, _ time.Duration) (int, error) {
	c.writes = append(c.writes, bytes.Clone(p))
	return len(p), nil
}

func (c *fakeConn) Close() error { c.closed = true; return nil }

func pkt(cmd uint32, length int, words ...uint32) []byte {
	b := make([]byte, length)
	binary.LittleEndian.PutUint32(b[0:4], cmd)
	binary.LittleEndian.PutUint32(b[4:8], uint32(length))
	for i, w := range words {
		binary.LittleEndian.PutUint32(b[8+i*4:12+i*4], w)
	}
	return b
}

func pkt64(cmd uint32, length int, words ...uint64) []byte {
	b := make([]byte, length)
	binary.LittleEndian.PutUint32(b[0:4], cmd)
	binary.LittleEndian.PutUint32(b[4:8], uint32(length))
	for i, w := range words {
		binary.LittleEndian.PutUint64(b[8+i*8:16+i*8], w)
	}
	return b
}

func TestUploadProgrammerHappyPath(t *testing.T) {
	prog := []byte("firehose-programmer-elf-bytes")
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen, 2, 1, 4096, 0),
		pkt(saharaReadData, saharaReadDataLen, programmerImageID, 0, uint32(len(prog))),
		pkt(saharaEndOfImage, saharaEndOfImageLen, programmerImageID, 0),
		pkt(saharaDoneResp, saharaDoneRespLen, 1),
	}}

	var sent, total int64
	if err := UploadProgrammer(c, "prog.elf", prog, func(s, tt int64) { sent, total = s, tt }); err != nil {
		t.Fatalf("UploadProgrammer: %v", err)
	}

	if len(c.writes) != 3 {
		t.Fatalf("wrote %d messages, want HELLO_RESP + payload + DONE", len(c.writes))
	}
	hello := c.writes[0]
	if got := binary.LittleEndian.Uint32(hello[0:4]); got != saharaHelloResp {
		t.Errorf("first write cmd = 0x%x, want HELLO_RESP", got)
	}
	if len(hello) != saharaHelloLen {
		t.Errorf("HELLO_RESP is %d bytes, want %d", len(hello), saharaHelloLen)
	}
	if got := binary.LittleEndian.Uint32(hello[8:12]); got != saharaVersion {
		t.Errorf("advertised version %d, want %d", got, saharaVersion)
	}
	if got := binary.LittleEndian.Uint32(hello[16:20]); got != 0 {
		t.Errorf("HELLO_RESP status = %d, want success", got)
	}
	if !bytes.Equal(c.writes[1], prog) {
		t.Errorf("payload write = %q, want the programmer bytes", c.writes[1])
	}
	if got := binary.LittleEndian.Uint32(c.writes[2][0:4]); got != saharaDone {
		t.Errorf("last write cmd = 0x%x, want DONE", got)
	}
	if sent != int64(len(prog)) || total != int64(len(prog)) {
		t.Errorf("progress reported %d/%d, want %d/%d", sent, total, len(prog), len(prog))
	}
}

func TestUploadProgrammerEchoesHelloMode(t *testing.T) {
	// Replying with our own mode instead of the device's leaves the two ends
	// disagreeing about what comes next.
	const deviceMode = 3
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen, 2, 1, 4096, deviceMode),
		pkt(saharaDoneResp, saharaDoneRespLen, 1),
	}}
	if err := UploadProgrammer(c, "prog.elf", []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(c.writes[0][20:24]); got != deviceMode {
		t.Errorf("HELLO_RESP mode = %d, want the device's %d", got, deviceMode)
	}
}

func TestUploadProgrammerServesChunkedReads(t *testing.T) {
	prog := []byte("0123456789")
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen, 2, 1, 4096, 0),
		pkt(saharaReadData, saharaReadDataLen, programmerImageID, 0, 4),
		pkt(saharaReadData, saharaReadDataLen, programmerImageID, 4, 6),
		pkt(saharaEndOfImage, saharaEndOfImageLen, programmerImageID, 0),
		pkt(saharaDoneResp, saharaDoneRespLen, 1),
	}}
	if err := UploadProgrammer(c, "prog.elf", prog, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.writes[1], prog[:4]) || !bytes.Equal(c.writes[2], prog[4:]) {
		t.Errorf("served %q then %q, want the requested ranges", c.writes[1], c.writes[2])
	}
}

func TestUploadProgrammerReadData64(t *testing.T) {
	prog := []byte("sixtyfour")
	c := &fakeConn{reads: [][]byte{
		pkt(saharaHello, saharaHelloLen, 2, 1, 4096, 0),
		pkt64(saharaReadData64, saharaReadData64Len, programmerImageID, 0, uint64(len(prog))),
		pkt(saharaEndOfImage, saharaEndOfImageLen, programmerImageID, 0),
		pkt(saharaDoneResp, saharaDoneRespLen, 1),
	}}
	if err := UploadProgrammer(c, "prog.elf", prog, nil); err != nil {
		t.Fatalf("UploadProgrammer: %v", err)
	}
	if !bytes.Equal(c.writes[1], prog) {
		t.Errorf("64-bit read served %q, want the programmer bytes", c.writes[1])
	}
}

func TestUploadProgrammerSkipsWhenProgrammerAlreadyRunning(t *testing.T) {
	// A device left in Firehose mode by an earlier flash says nothing at all.
	silent := &fakeConn{}
	if err := UploadProgrammer(silent, "prog.elf", []byte("x"), nil); err != nil {
		t.Fatalf("silent device: %v", err)
	}
	if len(silent.writes) != 0 {
		t.Errorf("wrote %d messages to a device already in Firehose mode", len(silent.writes))
	}

	// Or it answers the probe by failing to parse it as XML.
	xmlish := &fakeConn{reads: [][]byte{[]byte(`<?xml version="1.0"?><data><response value="NAK"/></data>`)}}
	if err := UploadProgrammer(xmlish, "prog.elf", []byte("x"), nil); err != nil {
		t.Fatalf("firehose banner: %v", err)
	}
}

func TestUploadProgrammerRejectsBadDevice(t *testing.T) {
	prog := []byte("0123456789")
	hello := pkt(saharaHello, saharaHelloLen, 2, 1, 4096, 0)

	for name, tc := range map[string]struct {
		reads [][]byte
		want  string
	}{
		"image rejected": {
			reads: [][]byte{hello, pkt(saharaEndOfImage, saharaEndOfImageLen, programmerImageID, 5)},
			want:  "rejected the programmer image",
		},
		"unknown image id": {
			reads: [][]byte{hello, pkt(saharaReadData, saharaReadDataLen, 99, 0, 4)},
			want:  "requested sahara image 99",
		},
		"read past end": {
			reads: [][]byte{hello, pkt(saharaReadData, saharaReadDataLen, programmerImageID, 8, 99)},
			want:  "of a 10-byte programmer",
		},
		"declared length mismatch": {
			reads: [][]byte{append(pkt(saharaHello, saharaHelloLen), 0xff)},
			want:  "declares 48 bytes but 49 arrived",
		},
		"wants more images": {
			reads: [][]byte{hello, pkt(saharaDoneResp, saharaDoneRespLen, 0)},
			want:  "more Sahara images",
		},
		"session reset": {
			reads: [][]byte{hello, pkt(saharaResetResp, saharaResetLen)},
			want:  "reset the sahara session",
		},
		"short hello": {
			reads: [][]byte{pkt(saharaHello, 12)},
			want:  "HELLO has length 12",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeConn{reads: tc.reads}
			err := UploadProgrammer(c, "prog.elf", prog, nil)
			if err == nil {
				t.Fatalf("want an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestUploadProgrammerReportsMidStreamTimeout(t *testing.T) {
	// A timeout on the FIRST read means "already in Firehose"; later it is a
	// genuine stall and must not be mistaken for success.
	c := &fakeConn{reads: [][]byte{pkt(saharaHello, saharaHelloLen, 2, 1, 4096, 0)}}
	err := UploadProgrammer(c, "prog.elf", []byte("x"), nil)
	if err == nil {
		t.Fatal("want an error for a stall after HELLO, got nil")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error %q should wrap ErrTimeout", err)
	}
}
