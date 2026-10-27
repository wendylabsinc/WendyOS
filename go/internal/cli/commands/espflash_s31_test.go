package commands

import (
	"encoding/binary"
	"reflect"
	"testing"
)

type s31ROMPort struct {
	scriptedReadPort
	writes [][2]uint32
}

func (p *s31ROMPort) Write(data []byte) (int, error) {
	// Decode the command with the production SLIP decoder, including escaped
	// register values. Return ROM responses for discovery and watchdog writes.
	in := &scriptedReadPort{input: append([]byte(nil), data...)}
	f := &espFlasher{port: in}
	frame, err := f.slipDecode()
	if err != nil {
		return 0, err
	}
	op := frame[1]
	response := make([]byte, 12)
	response[0] = 1
	response[1] = op
	if op == espCmdGetSecurityInfo {
		response = make([]byte, 32)
		response[0] = 1
		response[1] = op
		binary.LittleEndian.PutUint32(response[20:24], 32)
	} else if op == espCmdWriteReg {
		p.writes = append(p.writes, [2]uint32{binary.LittleEndian.Uint32(frame[8:12]), binary.LittleEndian.Uint32(frame[12:16])})
	}
	binary.LittleEndian.PutUint16(response[2:4], uint16(len(response)-8))
	p.input = append(p.input, slipEncode(response)...)
	return len(data), nil
}

func TestESP32S31ROMDiscoveryAndReset(t *testing.T) {
	p := &s31ROMPort{}
	f := &espFlasher{port: p}
	if err := f.detectChip(); err != nil {
		t.Fatal(err)
	}
	if f.chip != chipESP32S31 || f.regs != &regsESP32S31 {
		t.Fatalf("wrong chip: %+v", f)
	}
	if err := validateDetectedChip(chipESP32S3, f.chip); err == nil {
		t.Fatal("S3 firmware accepted for S31")
	}
	if err := f.watchdogReset(); err != nil {
		t.Fatal(err)
	}
	want := [][2]uint32{{0x20801018, 0x50d83aa1}, {0x20801004, 2000}, {0x20801000, 0xd0000102}, {0x20801018, 0}}
	if !reflect.DeepEqual(p.writes, want) {
		t.Fatalf("reset writes %x, want %x", p.writes, want)
	}
}
