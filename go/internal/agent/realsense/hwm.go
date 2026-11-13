package realsense

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// The HWM (hardware monitor) wire format, per librealsense's
// command_transfer_over_xu: both directions are fixed 1024-byte frames on the
// depth function's extension unit (unit 3, selector 1 — DS5_HWMONITOR). The
// kernel refuses any other transfer size for this control (-ENOBUFS), so the
// frame size is not negotiable.
const (
	hwmFrameSize  = 1024
	hwmMagic      = 0xCDAB
	hwmDataSizeAt = 1020 // u32: payload length, in the reply frame's tail

	opGVD          = 0x10 // identity: firmware, serial, sensor presence
	opGETINTCAL    = 0x15 // read a calibration table by id
	opGETADV       = 0x2C // advanced-mode read (param 9 = depth units, µm)
	opUAMG         = 0x30 // advanced-mode availability
	opRECPARAMSGET = 0x7E // per-resolution rectified params (fw >= 5.11.9.5)

	tableCoefficients = 25 // depth: baseline + per-resolution intrinsics
	tableRGB          = 32 // colour: intrinsics + depth<->colour extrinsics
)

// hwmErrors names the device's negative reply codes (librealsense
// hwmon_response). Anything unlisted is reported numerically.
var hwmErrors = map[int32]string{
	-6:  "ERR_INVALID_PARAM",
	-16: "ERR_GET_INVALID_PARAM",
	-25: "TABLE_IS_EMPTY",
	-28: "WRONG_TABLE_ID",
}

// buildCommand frames one HWM command. Offset 0 holds the bytes written minus
// four (20 for a parameter-only command), 2 the 0xCDAB magic, 4 the opcode,
// 8 and 12 the two parameters.
func buildCommand(opcode, p1, p2 uint32) []byte {
	tx := make([]byte, hwmFrameSize)
	binary.LittleEndian.PutUint16(tx[0:], 20)
	binary.LittleEndian.PutUint16(tx[2:], hwmMagic)
	binary.LittleEndian.PutUint32(tx[4:], opcode)
	binary.LittleEndian.PutUint32(tx[8:], p1)
	binary.LittleEndian.PutUint32(tx[12:], p2)
	return tx
}

// parseReply validates a raw 1024-byte reply frame against the opcode it
// answers and returns its payload. The first four bytes echo the opcode on
// success and carry a negative error code otherwise; the payload length lives
// at offset 1020 and is clamped to what the frame can actually hold.
func parseReply(rx []byte, opcode uint32) ([]byte, error) {
	if len(rx) != hwmFrameSize {
		return nil, fmt.Errorf("realsense: HWM reply is %d bytes, want %d", len(rx), hwmFrameSize)
	}
	echo := int32(binary.LittleEndian.Uint32(rx[0:]))
	if uint32(echo) != opcode {
		if name, ok := hwmErrors[echo]; ok {
			return nil, fmt.Errorf("realsense: op 0x%02x: device error %s (%d)", opcode, name, echo)
		}
		return nil, fmt.Errorf("realsense: op 0x%02x: device error %d", opcode, echo)
	}
	n := binary.LittleEndian.Uint32(rx[hwmDataSizeAt:])
	if n > hwmDataSizeAt-4 {
		n = hwmDataSizeAt - 4
	}
	return rx[4 : 4+n], nil
}

// tableHeader is the 16-byte prefix of every GETINTCAL table.
type tableHeader struct {
	Major, Minor uint8
	TableSize    uint32 // bytes after the header, per the device
	CRC          uint32
}

// checkTable validates a calibration table's header and CRC and returns the
// header. minLen is the last offset this package will actually read, NOT the
// SDK struct's sizeof: firmware trims trailing reserved bytes (fw 5.15.1.55
// returns table 25 as 512 bytes against a 576-byte struct), so the gate is
// the content we consume. The CRC is IEEE over the bytes after the header —
// some firmware covers everything it sent, some exactly TableSize bytes, and
// either is accepted.
func checkTable(payload []byte, minLen int) (tableHeader, error) {
	if len(payload) < 16 {
		return tableHeader{}, fmt.Errorf("realsense: table is %d bytes, shorter than its own header", len(payload))
	}
	// The version word is the format's one big-endian field.
	ver := binary.BigEndian.Uint16(payload[0:])
	h := tableHeader{
		Major:     uint8(ver >> 8),
		Minor:     uint8(ver & 0xff),
		TableSize: binary.LittleEndian.Uint32(payload[4:]),
		CRC:       binary.LittleEndian.Uint32(payload[12:]),
	}
	if len(payload) < minLen {
		return h, fmt.Errorf("realsense: table v%d.%d is %d bytes, need %d for the fields read here",
			h.Major, h.Minor, len(payload), minLen)
	}
	if crc32.ChecksumIEEE(payload[16:]) != h.CRC {
		if int(h.TableSize)+16 > len(payload) || crc32.ChecksumIEEE(payload[16:16+h.TableSize]) != h.CRC {
			return h, fmt.Errorf("realsense: table v%d.%d CRC mismatch (header %08x)", h.Major, h.Minor, h.CRC)
		}
	}
	return h, nil
}
