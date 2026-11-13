//go:build darwin || linux || windows

package commands

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/seriallock"
	"go.bug.st/serial"
)

// ESP32 bootloader command opcodes.
const (
	espCmdFlashBegin      = 0x02
	espCmdFlashData       = 0x03
	espCmdFlashEnd        = 0x04
	espCmdSync            = 0x08
	espCmdWriteReg        = 0x09
	espCmdReadReg         = 0x0A
	espCmdSPISetParams    = 0x0B
	espCmdSPIAttach       = 0x0D
	espCmdChangeBaud      = 0x0F
	espCmdGetSecurityInfo = 0x14
)

// chipModel identifies the ESP32 variant detected from the bootloader.
type chipModel int

const (
	chipUnknown chipModel = iota
	chipESP32C5
	chipESP32C6
	chipESP32P4
	chipESP32S3
	chipESP32C61
	chipESP32S31
)

func chipModelForTarget(target string) (chipModel, error) {
	switch target {
	case "esp32c5":
		return chipESP32C5, nil
	case "esp32c6":
		return chipESP32C6, nil
	case "esp32p4":
		return chipESP32P4, nil
	case "esp32s3":
		return chipESP32S3, nil
	case "esp32s31":
		return chipESP32S31, nil
	case "esp32c61":
		return chipESP32C61, nil
	default:
		return chipUnknown, fmt.Errorf("unsupported ESP32 firmware target %q", target)
	}
}

func chipModelName(chip chipModel) string {
	switch chip {
	case chipESP32C5:
		return "ESP32-C5"
	case chipESP32C6:
		return "ESP32-C6"
	case chipESP32P4:
		return "ESP32-P4"
	case chipESP32S3:
		return "ESP32-S3"
	case chipESP32S31:
		return "ESP32-S31"
	case chipESP32C61:
		return "ESP32-C61"
	default:
		return "unknown ESP32"
	}
}

func validateDetectedChip(expected, detected chipModel) error {
	if expected != detected {
		return fmt.Errorf("connected device is %s, but the selected firmware targets %s", chipModelName(detected), chipModelName(expected))
	}
	return nil
}

// chipRegs holds chip-specific peripheral register addresses. Different ESP32
// variants have incompatible memory maps, so all chip-sensitive code goes
// through f.regs rather than hardcoded constants.
type chipRegs struct {
	name string
	// Watchdog registers: MWDT and SWD are disabled before flashing.
	wdtProtect uint32 // MWDT write-protect key register
	wdtConfig0 uint32 // MWDT config0 (write 0 to disable)
	swdProtect uint32 // SWD write-protect key register
	swdConf    uint32 // SWD config register
	// SWD unlock key and auto-feed-enable bit: both differ between the older
	// RTC_CNTL-based SWD (ESP32-S3) and the newer unified LP_WDT block
	// (C5/C6/P4), unlike every other register above which is just an address.
	swdWkey       uint32 // SWD write-protect unlock key
	swdAutoFeedEn uint32 // bit to OR into swdConf to auto-feed (disable) the super watchdog
	// eFuse registers.
	efuseA  uint32 // BLOCK0 misc register A
	efuseB  uint32 // BLOCK0 misc register B
	chipID0 uint32 // eFuse chip ID word 0
	chipID1 uint32 // eFuse chip ID word 1
	macLow  uint32 // eFuse MAC address low word
	macHigh uint32 // eFuse MAC address high word
	// SPI flash controller registers (register naming follows esptool offsets).
	spiCmd      uint32 // SPI_MEM_CMD_REG      (offset +0x00)
	spiUser     uint32 // SPI_MEM_USER_REG     (offset +0x18)
	spiUser2    uint32 // SPI_MEM_USER2_REG    (offset +0x20) — holds the command opcode
	spiMisoDlen uint32 // SPI_MEM_MISO_DLEN_REG (offset +0x28) — read_bits-1 before a read-phase command
	spiW0       uint32 // SPI_MEM_W0_REG       (offset +0x58)
}

// ESP32-C5 shares WDT and SPI registers with C6; only the eFuse base differs.
var regsESP32C5 = chipRegs{
	name:          "ESP32-C5",
	wdtProtect:    0x600b1c18,
	wdtConfig0:    0x600b1c00,
	swdProtect:    0x600b1c20,
	swdConf:       0x600b1c1c,
	swdWkey:       0x50d83aa1,
	swdAutoFeedEn: 0x00040000, // 1<<18
	efuseA:        0x600b4830,
	efuseB:        0x600b4838,
	chipID0:       0x600b4850,
	chipID1:       0x600b4854,
	macLow:        0x600b4844,
	macHigh:       0x600b4848,
	spiCmd:        0x60003000,
	spiUser:       0x60003018,
	spiUser2:      0x60003020,
	spiMisoDlen:   0x60003028,
	spiW0:         0x60003058,
}

// ESP32-S31 ROM addresses, verified against esptool 5.6.0 esp32s31.py.
var regsESP32S31 = chipRegs{
	name:          "ESP32-S31",
	wdtProtect:    0x20801018,
	wdtConfig0:    0x20801000,
	swdProtect:    0x20801020,
	swdConf:       0x2080101c,
	swdWkey:       0x50d83aa1,
	swdAutoFeedEn: 0x00040000, // 1<<18
	efuseA:        0x20715030,
	efuseB:        0x20715038,
	chipID0:       0x20715050,
	chipID1:       0x20715054,
	macLow:        0x20715050,
	macHigh:       0x20715054,
	spiCmd:        0x20501000,
	spiUser:       0x20501018,
	spiUser2:      0x20501020,
	spiMisoDlen:   0x20501028,
	spiW0:         0x20501058,
}

var regsESP32C6 = chipRegs{
	name:          "ESP32-C6",
	wdtProtect:    0x600b1c18,
	wdtConfig0:    0x600b1c00,
	swdProtect:    0x600b1c20,
	swdConf:       0x600b1c1c,
	swdWkey:       0x50d83aa1,
	swdAutoFeedEn: 0x00040000, // 1<<18
	efuseA:        0x600b0830,
	efuseB:        0x600b0838,
	chipID0:       0x600b0850,
	chipID1:       0x600b0854,
	macLow:        0x600b0844,
	macHigh:       0x600b0848,
	spiCmd:        0x60003000,
	spiUser:       0x60003018,
	spiUser2:      0x60003020,
	spiMisoDlen:   0x60003028,
	spiW0:         0x60003058,
}

var regsESP32P4 = chipRegs{
	name:          "ESP32-P4",
	wdtProtect:    0x50116018,
	wdtConfig0:    0x50116000,
	swdProtect:    0x50116020,
	swdConf:       0x5011601c,
	swdWkey:       0x50d83aa1,
	swdAutoFeedEn: 0x00040000, // 1<<18
	efuseA:        0x5012d030,
	efuseB:        0x5012d038,
	chipID0:       0x5012d050,
	chipID1:       0x5012d054,
	macLow:        0x5012d044,
	macHigh:       0x5012d048,
	spiCmd:        0x5008d000,
	spiUser:       0x5008d018,
	spiUser2:      0x5008d020,
	spiMisoDlen:   0x5008d028,
	spiW0:         0x5008d058,
}

// ESP32-S3 predates the unified LP_WDT block the other chips here share: its
// main/super watchdog both live in RTC_CNTL at different offsets, its SWD
// unlock key/auto-feed bit differ from the RISC-V chips', and its SPI1 base
// differs too. Verified against ESP-IDF's esp32s3 SoC headers and esptool's
// esp32s3.py target.
var regsESP32S3 = chipRegs{
	name:          "ESP32-S3",
	wdtProtect:    0x600080b0,
	wdtConfig0:    0x60008098,
	swdProtect:    0x600080b8,
	swdConf:       0x600080b4,
	swdWkey:       0x8f1d312a,
	swdAutoFeedEn: 0x80000000, // 1<<31
	efuseA:        0x60007030,
	efuseB:        0x60007038,
	chipID0:       0x60007050,
	chipID1:       0x60007054,
	macLow:        0x60007044,
	macHigh:       0x60007048,
	spiCmd:        0x60002000,
	spiUser:       0x60002018,
	spiUser2:      0x60002020,
	spiMisoDlen:   0x60002028,
	spiW0:         0x60002058,
}

// ESP32-C61 shares C6's unified LP_WDT block (same base address, same SWD
// key/auto-feed bit) and its SPI1 controller is byte-for-byte identical to
// C6's too -- verified against the ESP-IDF v5.5.4 register headers
// (spi1_mem_reg.h, lp_wdt_reg.h). Only the eFuse base differs, matching C5's
// instead of C6's.
var regsESP32C61 = chipRegs{
	name:          "ESP32-C61",
	wdtProtect:    0x600b1c18,
	wdtConfig0:    0x600b1c00,
	swdProtect:    0x600b1c20,
	swdConf:       0x600b1c1c,
	swdWkey:       0x50d83aa1,
	swdAutoFeedEn: 0x00040000, // 1<<18
	efuseA:        0x600b4830,
	efuseB:        0x600b4838,
	chipID0:       0x600b4850,
	chipID1:       0x600b4854,
	macLow:        0x600b4844,
	macHigh:       0x600b4848,
	spiCmd:        0x60003000,
	spiUser:       0x60003018,
	spiUser2:      0x60003020,
	spiMisoDlen:   0x60003028,
	spiW0:         0x60003058,
}

// SLIP framing bytes.
const (
	slipEnd    = 0xC0
	slipEsc    = 0xDB
	slipEscEnd = 0xDC
	slipEscEsc = 0xDD
)

const (
	espFlashBlockSize = 0x1000            // 4 KiB per flash data block
	maxFlashSize      = 128 * 1024 * 1024 // 128 MiB, generous upper bound for NOR flash
	espSyncTimeout    = 1 * time.Second
	espCmdTimeout     = 10 * time.Second
	flashBaudRate     = 921600
	initialBaudRate   = 115200
)

const espDebugEnabled = false

func dbgf(format string, args ...any) {
	if espDebugEnabled {
		fmt.Printf("DEBUG "+format+"\r\n", args...)
	}
}

// JedecID holds the three-byte JEDEC flash identification returned by the
// RDID (0x9f) command.
type JedecID struct {
	manufacturer byte // vendor code (e.g. 0xEF = Winbond, 0x20 = Micron)
	memoryType   byte // memory technology and interface (e.g. 0x40 = SPI NOR)
	capacity     byte // density code (e.g. 0x17 = 64 Mbit)
}

// espFlasher handles serial communication with the ESP32 bootloader.
type espFlasher struct {
	port        serial.Port
	chip        chipModel
	regs        *chipRegs
	readTimeout time.Duration
}

func isPermissionDenied(err error) bool {
	var portErr *serial.PortError
	if errors.As(err, &portErr) && portErr.Code() == serial.PermissionDenied {
		return true
	}
	// openLockedPort's own pre-open (via seriallock.Acquire) fails with a
	// raw syscall error, not a *serial.PortError, when it can't even open
	// the device to take the lock.
	return errors.Is(err, fs.ErrPermission)
}

// errPortBusy indicates the serial port is already open by another process.
// Wrapped into the error chain wherever go.bug.st/serial reports
// serial.PortBusy, so callers can detect this specific failure mode via
// errors.Is regardless of how many layers the error has been wrapped
// through.
var errPortBusy = errors.New("serial port busy")

func isPortBusy(err error) bool {
	var portErr *serial.PortError
	return errors.As(err, &portErr) && portErr.Code() == serial.PortBusy
}

func espLoaderErrorMessage(code byte) string {
	switch code {
	case 0x00:
		return "undefined error"
	case 0x01:
		return "invalid input parameter"
	case 0x02:
		return "failed to allocate memory"
	case 0x03:
		return "failed to send message"
	case 0x04:
		return "failed to receive message"
	case 0x05:
		return "invalid message format"
	case 0x06:
		return "bad execution result"
	case 0x07:
		return "checksum error"
	case 0x08:
		return "flash write error (CRC mismatch on readback)"
	case 0x09:
		return "flash read error"
	case 0x0a:
		return "flash read length error"
	case 0x0b:
		return "deflate error"
	case 0x0c:
		return "deflate Adler32 error"
	case 0x0d:
		return "deflate parameter error"
	case 0x0e:
		return "invalid RAM binary size"
	case 0x0f:
		return "invalid RAM binary address"
	case 0x64:
		return "invalid parameter"
	case 0x65:
		return "invalid format"
	case 0x66:
		return "description too long"
	case 0x67:
		return "bad encoding description"
	case 0x69:
		return "insufficient storage"
	default:
		return fmt.Sprintf("unknown error code 0x%02x", code)
	}
}

func flashSize(id JedecID) uint32 {
	const defaultSize = 4 * 1024 * 1024
	if id.capacity == 0 {
		return defaultSize
	}
	if id.capacity > 31 {
		return maxFlashSize
	}
	size := uint32(1) << id.capacity
	if size > maxFlashSize {
		return maxFlashSize
	}
	return size
}

func slipEncode(data []byte) []byte {
	buf := make([]byte, 0, len(data)*2+2)
	buf = append(buf, slipEnd)
	for _, b := range data {
		switch b {
		case slipEnd:
			buf = append(buf, slipEsc, slipEscEnd)
		case slipEsc:
			buf = append(buf, slipEsc, slipEscEsc)
		default:
			buf = append(buf, b)
		}
	}
	buf = append(buf, slipEnd)
	return buf
}

// readByte reads exactly one byte from the serial port, retrying on
// zero-length reads (which go.bug.st/serial returns on timeout instead
// of an error).
func (f *espFlasher) readByte() (byte, error) {
	buf := make([]byte, 1)
	timeout := f.readTimeout
	if timeout <= 0 {
		timeout = espCmdTimeout
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := f.port.Read(buf)
		if err != nil {
			return 0, err
		}
		if n == 1 {
			return buf[0], nil
		}
		// n == 0: port timeout, but our deadline hasn't passed — retry.
	}
	return 0, fmt.Errorf("serial read timed out")
}

func (f *espFlasher) setReadTimeout(timeout time.Duration) {
	f.readTimeout = timeout
	// Preserve the existing behavior of surfacing any ensuing Read failure;
	// go.bug.st/serial only reports invalid timeouts here, and all callers use
	// positive constants.
	_ = f.port.SetReadTimeout(timeout)
}

// Ensure that all bytes are sent.
func (f *espFlasher) writeData(data []byte) error {
	for len(data) > 0 {
		n, err := f.port.Write(data)
		if err != nil {
			return fmt.Errorf("write data error: %w", err)
		}
		data = data[n:]
	}
	return nil
}

func (f *espFlasher) slipDecode() ([]byte, error) {
	for {
		// Scan for the start-of-frame marker (0xC0).
		for {
			b, err := f.readByte()
			if err != nil {
				return nil, err
			}
			if b == slipEnd {
				break
			}
		}

		// Read until the end-of-frame marker.
		var frame []byte
		escaped := false
		for {
			b, err := f.readByte()
			if err != nil {
				return nil, err
			}

			if escaped {
				switch b {
				case slipEscEnd:
					frame = append(frame, slipEnd)
				case slipEscEsc:
					frame = append(frame, slipEsc)
				default:
					// Invalid escape sequence — include as-is.
					frame = append(frame, b)
				}
				escaped = false
				continue
			}

			switch b {
			case slipEnd:
				// End of frame. With consecutive delimiters this byte is also
				// the start of the next frame, so remain inside the frame-reading
				// loop. Returning to the outer start-marker scan would consume the
				// next frame's data as garbage and discard its closing delimiter.
				if len(frame) > 0 {
					return frame, nil
				}
				continue
			case slipEsc:
				escaped = true
			default:
				frame = append(frame, b)
			}
		}
	}
}

// buildCommand constructs an ESP bootloader command packet.
func buildCommand(opcode byte, data []byte, checksum byte) []byte {
	// Header: direction(1) + command(1) + size(2) + checksum(4)
	pkt := make([]byte, 8+len(data))
	pkt[0] = 0x00 // direction: request
	pkt[1] = opcode
	binary.LittleEndian.PutUint16(pkt[2:4], uint16(len(data)))
	binary.LittleEndian.PutUint32(pkt[4:8], uint32(checksum))
	copy(pkt[8:], data)
	return pkt
}

// sendCommand sends a command and reads the matching response,
// skipping any stale frames that don't match the expected opcode.
func (f *espFlasher) sendCommand(opcode byte, data []byte, checksum byte) ([]byte, error) {
	pkt := buildCommand(opcode, data, checksum)
	encoded := slipEncode(pkt)

	if err := f.writeData(encoded); err != nil {
		return nil, fmt.Errorf("writing command 0x%02x: %w", opcode, err)
	}

	// Try to read a valid response, skipping stale/mismatched frames.
	for attempt := 0; attempt < 10; attempt++ {
		resp, err := f.slipDecode()
		if err != nil {
			return nil, fmt.Errorf("reading response for 0x%02x: %w", opcode, err)
		}

		if len(resp) < 8 {
			// Too short — likely garbage, skip it.
			continue
		}

		// Check direction byte (0x01 = response from bootloader).
		if resp[0] != 0x01 {
			continue
		}

		// Check command echo matches what we sent.
		if resp[1] != opcode {
			// Response for a different command — skip (stale from previous).
			continue
		}

		// Check payload
		payload := resp[8:]
		if len(payload) < 2 {
			return nil, fmt.Errorf("bad protocol: response for 0x%02x too short (%d bytes)", opcode, len(payload))
		}
		if payload[0] != 0 || payload[1] != 0 {
			if payload[0] != 1 {
				return nil, fmt.Errorf("bad protocol: unexpected status 0x%02x for command 0x%02x", payload[0], opcode)
			}
			return nil, fmt.Errorf("command 0x%02x rejected: %s", opcode, espLoaderErrorMessage(payload[1]))
		}
		return resp[4:], nil
	}

	return nil, fmt.Errorf("no valid response for 0x%02x after 10 frames", opcode)
}

// drain discards any pending data in the serial receive buffer. Using the
// driver's purge operation is important here: a running application may emit
// serial output continuously, so reading until a quiet interval can loop
// forever when a reset failed to enter the ROM bootloader.
func (f *espFlasher) drain() error {
	if err := f.port.ResetInputBuffer(); err != nil {
		return fmt.Errorf("resetting serial input buffer: %w", err)
	}
	return nil
}

// sync synchronizes with the ESP32 bootloader.
func (f *espFlasher) sync() error {
	// Sync frame: 0x07 0x07 0x12 0x20 + 32 bytes of 0x55
	data := make([]byte, 36)
	data[0] = 0x07
	data[1] = 0x07
	data[2] = 0x12
	data[3] = 0x20
	for i := 4; i < 36; i++ {
		data[i] = 0x55
	}

	for attempt := 0; attempt < 10; attempt++ {
		f.setReadTimeout(espSyncTimeout)
		_, err := f.sendCommand(espCmdSync, data, 0)
		if err == nil {
			// Drain extra sync responses (bootloader sends multiple).
			if err := f.drain(); err != nil {
				return err
			}
			f.setReadTimeout(espCmdTimeout)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("failed to sync with ESP32 bootloader after 10 attempts")
}

// changeBaudRate switches the bootloader to a faster baud rate.
func (f *espFlasher) changeBaudRate(newBaud int) error {
	data := changeBaudCommandData(newBaud)

	f.setReadTimeout(espCmdTimeout)
	if _, err := f.sendCommand(espCmdChangeBaud, data, 0); err != nil {
		return fmt.Errorf("changing baud rate: %w", err)
	}

	// Drain any data still at the old baud rate before switching.
	if err := f.drain(); err != nil {
		return err
	}

	// Reconfigure the serial port to the new baud rate.
	if err := f.port.SetMode(&serial.Mode{
		BaudRate: newBaud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}); err != nil {
		return fmt.Errorf("reconfiguring serial port: %w", err)
	}

	// Wait for the bootloader to settle at the new rate, then drain
	// any transition garbage.
	time.Sleep(100 * time.Millisecond)
	if err := f.drain(); err != nil {
		return err
	}

	return nil
}

func changeBaudCommandData(newBaud int) []byte {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[0:4], uint32(newBaud))
	// The second argument is the old baud only when talking to an uploaded
	// flasher stub. Wendy talks directly to the ROM bootloader, which requires
	// this field to be zero (matching esptool's ESPLoader.change_baud).
	binary.LittleEndian.PutUint32(data[4:8], 0)
	return data
}

// detectChip sends GET_SECURITY_INFO (0x14), extracts the chip_id from the
// response, and populates f.chip and f.regs. Must be called before any
// chip-specific register access.
//
// Response layout (sendCommand returns frame[4:]):
//
//	[0:4]  value field (unused for this command)
//	[4:]   security info payload (20 bytes), status bytes follow after:
//	  [4:8]   flags
//	  [8]     flash_crypt_cnt
//	  [9:16]  key_purposes[7]
//	  [16:20] chip_id (uint32 LE)
//	  [20:24] api_version
//	[24:26] status bytes (esptool places them after the data)
func (f *espFlasher) detectChip() error {
	f.setReadTimeout(espCmdTimeout)
	resp, err := f.sendCommand(espCmdGetSecurityInfo, nil, 0)
	if err != nil {
		return fmt.Errorf("get security info: %w", err)
	}
	const chipIDOff = 4 + 12 // value(4) + flags(4) + flash_crypt_cnt(1) + key_purposes(7)
	if len(resp) < chipIDOff+4 {
		return fmt.Errorf("get security info: response too short (%d bytes)", len(resp))
	}
	chipID := binary.LittleEndian.Uint32(resp[chipIDOff : chipIDOff+4])
	dbgf("detectChip: chipID=0x%04x", chipID)
	switch chipID {
	case 0x0017:
		f.chip = chipESP32C5
		f.regs = &regsESP32C5
	case 0x000d:
		f.chip = chipESP32C6
		f.regs = &regsESP32C6
	case 0x0012:
		f.chip = chipESP32P4
		f.regs = &regsESP32P4
	case 0x0009:
		f.chip = chipESP32S3
		f.regs = &regsESP32S3
	case 0x0020:
		f.chip = chipESP32S31
		f.regs = &regsESP32S31
	case 0x0014:
		f.chip = chipESP32C61
		f.regs = &regsESP32C61
	default:
		return fmt.Errorf("unsupported chip id 0x%04x", chipID)
	}
	dbgf("detectChip: chipName=%s", f.regs.name)
	return nil
}

// readReg reads a 32-bit peripheral register at addr.
// The ROM bootloader returns the value in the header value field (bytes 4–7
// of the raw response), which sendCommand exposes as result[0:4].
func (f *espFlasher) readReg(addr uint32) (uint32, error) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, addr)
	f.setReadTimeout(espCmdTimeout)
	result, err := f.sendCommand(espCmdReadReg, data, 0)
	if err != nil {
		return 0, err
	}
	if len(result) < 4 {
		return 0, fmt.Errorf("readReg 0x%08x: short response", addr)
	}
	return binary.LittleEndian.Uint32(result[0:4]), nil
}

// writeReg performs a masked write to a 32-bit peripheral register:
//
//	reg[addr] = (reg[addr] & ^mask) | (value & mask)
//
// delay is a post-write delay in microseconds (pass 0 for no delay).
func (f *espFlasher) writeReg(addr, value, mask, delay uint32) error {
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data[0:4], addr)
	binary.LittleEndian.PutUint32(data[4:8], value)
	binary.LittleEndian.PutUint32(data[8:12], mask)
	binary.LittleEndian.PutUint32(data[12:16], delay)
	f.setReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdWriteReg, data, 0)
	return err
}

// spiAttach attaches the SPI flash.
func (f *espFlasher) spiAttach() error {
	data := make([]byte, 8)
	f.setReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdSPIAttach, data, 0)
	return err
}

// initChip disables the hardware watchdogs (MWDT and SWD) using chip-specific
// register addresses, then performs the eFuse debug reads observed in esptool
// traces. f.regs must be set by detectChip before calling this.
func (f *espFlasher) initChip() error {
	r := f.regs

	// MWDT: unlock → disable → re-lock.
	if err := f.writeReg(r.wdtProtect, 0x50d83aa1, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(r.wdtConfig0, 0x00000000, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(r.wdtProtect, 0x00000000, 0xffffffff, 0); err != nil {
		return err
	}
	dbgf("initChip: MWDT disabled OK")

	// SWD: unlock → OR in the auto-feed-enable bit → re-lock. Key and bit
	// position are chip-specific (see chipRegs doc).
	if err := f.writeReg(r.swdProtect, r.swdWkey, 0xffffffff, 0); err != nil {
		return err
	}
	val, err := f.readReg(r.swdConf)
	if err != nil {
		return err
	}
	dbgf("initChip: swdConf=0x%08x", val)
	if err := f.writeReg(r.swdConf, val|r.swdAutoFeedEn, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(r.swdProtect, 0x00000000, 0xffffffff, 0); err != nil {
		return err
	}
	dbgf("initChip: SWD disabled OK")

	// Debug: eFuse chip-ID and MAC reads (x3 each, matching esptool trace cadence).
	for i := 0; i < 3; i++ {
		v, err := f.readReg(r.chipID0)
		if err != nil {
			return err
		}
		dbgf("initChip: chipID0[%d]=0x%08x", i, v)
	}
	v, err := f.readReg(r.chipID1)
	if err != nil {
		return err
	}
	dbgf("initChip: chipID1=0x%08x", v)
	for i := 0; i < 3; i++ {
		lo, err := f.readReg(r.macLow)
		if err != nil {
			return err
		}
		hi, err := f.readReg(r.macHigh)
		if err != nil {
			return err
		}
		dbgf("initChip: MAC[%d] lo=0x%08x hi=0x%08x", i, lo, hi)
	}

	return nil
}

// waitSPICmd polls SPI_MEM_CMD_REG until the SPI_USR bit (bit 18) is cleared,
// indicating the hardware has finished executing the command.
func (f *espFlasher) waitSPICmd() error {
	const timeout = 4000 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		val, err := f.readReg(f.regs.spiCmd)
		if err != nil {
			return err
		}
		if val&0x00040000 == 0 {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("SPI command timeout")
}

// initFlashChip performs the SPI flash controller register sequence
// observed in the esptool trace after SPI_ATTACH.
// It retrieves the JEDEC ID and resets the flash chip, in order to start
// without depending on previous uses.
func (f *espFlasher) initFlashChip() (JedecID, error) {
	r := f.regs
	user0, err := f.readReg(r.spiUser)
	if err != nil {
		return JedecID{}, err
	}
	user2, err := f.readReg(r.spiUser2)
	if err != nil {
		return JedecID{}, err
	}

	// Step 1: RDID (0x9f) — read the 24-bit JEDEC ID. 0x17 = 24-1 read bits.
	if err := f.writeReg(r.spiMisoDlen, 0x00000017, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser, 0x90000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser2, 0x7000009f, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiW0, 0x00000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiCmd, 0x00040000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.waitSPICmd(); err != nil {
		return JedecID{}, err
	}
	// W0 layout: bits 7:0 = Manufacturer, bits 15:8 = MemoryType, bits 23:16 = Capacity.
	w0, err := f.readReg(r.spiW0)
	if err != nil {
		return JedecID{}, err
	}
	id := JedecID{
		manufacturer: byte(w0),
		memoryType:   byte(w0 >> 8),
		capacity:     byte(w0 >> 16),
	}
	// Restore and verify.
	if err := f.writeReg(r.spiUser, user0, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser2, user2, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(r.spiUser); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(r.spiUser2); err != nil {
		return JedecID{}, err
	}

	// Step 2: RSTEN (0x66) — Reset Enable command.
	if err := f.writeReg(r.spiUser, 0x80000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser2, 0x70000066, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiW0, 0x00000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiCmd, 0x00040000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.waitSPICmd(); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(r.spiW0); err != nil {
		return JedecID{}, err
	}
	// Restore and verify.
	if err := f.writeReg(r.spiUser, user0, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser2, user2, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(r.spiUser); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(r.spiUser2); err != nil {
		return JedecID{}, err
	}

	// Step 3: RST (0x99) — Reset command.
	if err := f.writeReg(r.spiUser, 0x80000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser2, 0x70000099, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiW0, 0x00000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiCmd, 0x00040000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.waitSPICmd(); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(r.spiW0); err != nil {
		return JedecID{}, err
	}
	// Final restore (no verify needed after the last attempt).
	if err := f.writeReg(r.spiUser, user0, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(r.spiUser2, user2, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}

	return id, nil
}

// preFlashChecks performs the eFuse/security register reads observed on the esptool trace.
func (f *espFlasher) preFlashChecks() error {
	r := f.regs
	if _, err := f.readReg(r.efuseB); err != nil {
		return err
	}
	if _, err := f.readReg(r.chipID0); err != nil {
		return err
	}
	if _, err := f.readReg(r.chipID0); err != nil {
		return err
	}
	if _, err := f.readReg(r.efuseA); err != nil {
		return err
	}
	// Final check immediately before erase/flash-begin.
	if _, err := f.readReg(r.efuseB); err != nil {
		return err
	}
	return nil
}

// spiSetParams configures SPI flash parameters.
func (f *espFlasher) spiSetParams(totalSize uint32) error {
	data := make([]byte, 24)
	binary.LittleEndian.PutUint32(data[0:4], 0)         // id
	binary.LittleEndian.PutUint32(data[4:8], totalSize) // total size
	binary.LittleEndian.PutUint32(data[8:12], 64*1024)  // block size
	binary.LittleEndian.PutUint32(data[12:16], 4*1024)  // sector size
	binary.LittleEndian.PutUint32(data[16:20], 256)     // page size
	binary.LittleEndian.PutUint32(data[20:24], 0xFFFF)  // status mask

	f.setReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdSPISetParams, data, 0)
	return err
}

// eraseTimeout scales with image size: the ROM performs the erase in full,
// synchronously, before it ACKs FLASH_BEGIN, so a large image needs much
// longer than a small one. Mirrors esptool's own
// timeout_per_mb(ERASE_REGION_TIMEOUT_PER_MB=30, size).
func eraseTimeout(size uint32) time.Duration {
	const secondsPerMB = 30
	const floor = 3 * time.Second
	t := time.Duration(secondsPerMB * float64(size) / 1e6 * float64(time.Second))
	if t < floor {
		return floor
	}
	return t
}

// flashBegin starts a flash write operation, erasing the target region.
func (f *espFlasher) flashBegin(size, blockCount, blockSize, offset uint32) error {
	data := make([]byte, 20)
	binary.LittleEndian.PutUint32(data[0:4], size)
	binary.LittleEndian.PutUint32(data[4:8], blockCount)
	binary.LittleEndian.PutUint32(data[8:12], blockSize)
	binary.LittleEndian.PutUint32(data[12:16], offset)
	binary.LittleEndian.PutUint32(data[16:20], 0) // 0 = no encryption

	f.setReadTimeout(eraseTimeout(size)) // erase can be slow, scales with image size
	_, err := f.sendCommand(espCmdFlashBegin, data, 0)
	return err
}

// flashData sends a single block of flash data.
func (f *espFlasher) flashData(block []byte, seq uint32) error {
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(block)))
	binary.LittleEndian.PutUint32(header[4:8], seq)
	binary.LittleEndian.PutUint32(header[8:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], 0)

	data := append(header, block...)

	// Calculate checksum (XOR of all data bytes, seeded with 0xEF).
	var checksum byte = 0xEF
	for _, b := range block {
		checksum ^= b
	}

	f.setReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdFlashData, data, checksum)
	return err
}

// flashEnd finishes the flash operation.
func (f *espFlasher) flashEnd(reboot bool) error {
	data := make([]byte, 4)
	if !reboot {
		binary.LittleEndian.PutUint32(data, 1) // 1 = don't reboot
	}

	f.setReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdFlashEnd, data, 0)
	return err
}

// espResetViaUSBJTAG matches esptool's USBJTAGSerialReset strategy for the
// ESP32's native USB Serial/JTAG peripheral.
func espResetViaUSBJTAG(port serial.Port, enterBootloader bool) error {
	if err := port.SetRTS(false); err != nil {
		return fmt.Errorf("setting RTS idle: %w", err)
	}
	if err := port.SetDTR(false); err != nil {
		return fmt.Errorf("setting DTR idle: %w", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := port.SetDTR(enterBootloader); err != nil {
		return fmt.Errorf("setting DTR boot mode: %w", err)
	}
	if err := port.SetRTS(false); err != nil {
		return fmt.Errorf("holding RTS idle: %w", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := port.SetRTS(true); err != nil {
		return fmt.Errorf("asserting reset: %w", err)
	}
	if err := port.SetDTR(false); err != nil {
		return fmt.Errorf("releasing boot mode: %w", err)
	}
	// Repeat RTS for Windows usbser.sys, which may only propagate the updated
	// DTR value when RTS is set again.
	if err := port.SetRTS(true); err != nil {
		return fmt.Errorf("holding reset: %w", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := port.SetDTR(false); err != nil {
		return fmt.Errorf("holding DTR idle: %w", err)
	}
	if err := port.SetRTS(false); err != nil {
		return fmt.Errorf("releasing reset: %w", err)
	}
	return nil
}

// espResetViaUARTBridge matches esptool's ClassicReset strategy. On boards
// with a CP210x bridge, DTR drives GPIO0 and RTS drives EN through the board's
// auto-reset circuit; the native USB-JTAG sequence does not enter download
// mode on this hardware.
func espResetViaUARTBridge(port serial.Port, enterBootloader bool) error {
	if !enterBootloader {
		if err := port.SetRTS(true); err != nil {
			return fmt.Errorf("asserting reset: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
		if err := port.SetRTS(false); err != nil {
			return fmt.Errorf("releasing reset: %w", err)
		}
		return nil
	}

	if err := port.SetDTR(false); err != nil {
		return fmt.Errorf("releasing boot mode: %w", err)
	}
	if err := port.SetRTS(true); err != nil {
		return fmt.Errorf("asserting reset: %w", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := port.SetDTR(true); err != nil {
		return fmt.Errorf("selecting boot mode: %w", err)
	}
	if err := port.SetRTS(false); err != nil {
		return fmt.Errorf("releasing reset: %w", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := port.SetDTR(false); err != nil {
		return fmt.Errorf("releasing boot mode: %w", err)
	}
	return nil
}

func resetESP32(port serial.Port, enterBootloader bool, transport discovery.SerialTransport) error {
	if transport == discovery.SerialTransportUARTBridge {
		return espResetViaUARTBridge(port, enterBootloader)
	}
	return espResetViaUSBJTAG(port, enterBootloader)
}

// lockedPort pairs a serial.Port with the advisory flock acquired for it, so
// every existing f.port.Close() call releases both without any call site
// needing to change.
type lockedPort struct {
	serial.Port
	lock *seriallock.Lock
}

func (p *lockedPort) Close() error {
	err := p.Port.Close()
	p.lock.Release()
	return err
}

// openLockedPort acquires the WendyCom-style advisory flock for portPath —
// catching pyserial-based tools (idf.py monitor, esptool) that don't set
// TIOCEXCL and so would otherwise be invisible to go.bug.st/serial's own
// busy detection — before opening it, mirroring liteclient's
// ConnectToSerial. The lock is released whenever the returned port is
// Closed.
func openLockedPort(portPath string, mode *serial.Mode) (serial.Port, error) {
	lock, err := seriallock.Acquire(portPath)
	if err != nil {
		// Acquire fails for reasons besides "someone else holds it" — the
		// device doesn't exist yet, permission denied, ... — and only the
		// genuinely-locked case should be classified as busy: the others
		// need to stay retryable (missing device) or keep their
		// permission-denied messaging (isPermissionDenied below already
		// checks for this), not get labeled "busy".
		if errors.Is(err, seriallock.ErrLocked) {
			return nil, fmt.Errorf("%w: %s", errPortBusy, err)
		}
		return nil, err
	}
	port, err := serial.Open(portPath, mode)
	if err != nil {
		lock.Release()
		return nil, err
	}
	return &lockedPort{Port: port, lock: lock}, nil
}

// serialOpenFn opens a serial port; a package var so tests can stub it
// without touching real hardware.
var serialOpenFn = openLockedPort

// portOpenRetryBudget bounds how long openPortRetrying waits for a device
// node to (re)appear; portOpenRetryInterval is the poll spacing, matching
// esptool's connect_loop() port-open retry (esptool/__init__.py). Both are
// vars so tests can shrink them.
var (
	portOpenRetryBudget   = 5 * time.Second
	portOpenRetryInterval = 100 * time.Millisecond
)

// openPortRetrying opens portPath, retrying while the failure looks
// transient. Permission-denied is not transient — missing group membership
// won't resolve itself by waiting. Nor is a flock failure (errPortBusy,
// from openLockedPort): it means a different process on this host — an
// idf.py monitor session, say — holds the port, and nothing about us
// retrying will make it let go. Both return immediately, preserving the
// caller's specific messaging for those cases. Everything else, including
// raw kernel busy (TIOCEXCL), is retried until budget runs out: a device
// that's rebooting can make its USB node report busy for a moment (not just
// disappear as "no such file"), so that has to be retried rather than
// treated as a hard failure.
func openPortRetrying(portPath string, mode *serial.Mode, budget time.Duration) (serial.Port, error) {
	deadline := time.Now().Add(budget)
	for {
		port, err := serialOpenFn(portPath, mode)
		if err == nil {
			return port, nil
		}
		if isPermissionDenied(err) || errors.Is(err, errPortBusy) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(portOpenRetryInterval)
	}
}

// connectAttemptRetries bounds how many times connectAttempt re-pulses reset
// and retries sync. A single DTR/RTS pulse can miss a reboot-looping device's
// brief receptive window, so the whole reset+sync cycle is retried, not just
// sync itself. A var so tests can shrink it.
var connectAttemptRetries = 5

// connectAttempt opens portPath and gets the ESP32 bootloader to answer
// SYNC, retrying the whole reset-into-bootloader cycle up to
// connectAttemptRetries times when the device doesn't respond — not just
// the port open. This targets devices that are power-cycling or
// crash-looping on their own, where a single reset pulse can land while the
// chip is mid-crash rather than idle and listening.
//
// Native USB genuinely disconnects and re-enumerates after reset, so its port
// is closed and reopened after every pulse. A UART bridge remains enumerated
// while it resets the ESP32 behind it, so that transport retains the same
// locked handle across reset attempts.
func connectAttempt(portPath string, mode *serial.Mode, transport discovery.SerialTransport) (*espFlasher, error) {
	port, err := openPortRetrying(portPath, mode, portOpenRetryBudget)
	if err != nil {
		if isPermissionDenied(err) {
			if group := serialPortGroup(portPath); group != "" {
				return nil, fmt.Errorf("Permission denied to access USB device %s. To have access, you need to be part of the user group '%s'.", portPath, group)
			}
		}
		if errors.Is(err, errPortBusy) {
			// openLockedPort's flock error already names the port and the
			// likely holder, so it's returned as-is rather than re-wrapped.
			return nil, err
		}
		if isPortBusy(err) {
			// The underlying *serial.PortError just says "Serial port busy"
			// again, so it is dropped rather than repeated: errPortBusy already
			// carries that, and the path is the only new information.
			return nil, fmt.Errorf("%w: opening USB device %s", errPortBusy, portPath)
		}
		return nil, fmt.Errorf("opening USB device %s: %w", portPath, err)
	}
	f := &espFlasher{port: port}

	var lastErr error
	for attempt := 1; attempt <= connectAttemptRetries; attempt++ {
		if err := resetESP32(f.port, true, transport); err != nil {
			f.port.Close()
			return nil, fmt.Errorf("entering bootloader: %w", err)
		}

		if transport == discovery.SerialTransportUARTBridge {
			// The bridge remains enumerated while it resets the ESP32 itself.
			time.Sleep(100 * time.Millisecond)
		} else {
			f.port.Close()
			time.Sleep(500 * time.Millisecond)

			newPort, err := openPortRetrying(portPath, mode, portOpenRetryBudget)
			if err != nil {
				if errors.Is(err, errPortBusy) {
					return nil, err
				}
				if isPortBusy(err) {
					return nil, fmt.Errorf("%w: reopening port %s after reset", errPortBusy, portPath)
				}
				return nil, fmt.Errorf("reopening port after reset: %w", err)
			}
			f.port = newPort
		}

		if err := f.drain(); err != nil {
			lastErr = fmt.Errorf("draining serial input before sync: %w", err)
			continue
		}

		if err := f.sync(); err == nil {
			return f, nil
		} else {
			lastErr = err
		}
	}

	f.port.Close()
	return nil, fmt.Errorf("device did not respond after %d bootloader-reset attempts: %w", connectAttemptRetries, lastErr)
}

// flashFirmware is the main entry point: flash a .bin file to the ESP32.
func flashFirmware(portPath, firmwarePath string, transport discovery.SerialTransport, expectedChip chipModel, progressFn func(pct float64)) error {
	info, err := os.Stat(firmwarePath)
	if err != nil {
		return fmt.Errorf("reading firmware: %w", err)
	}
	if info.Size() > maxFlashSize {
		return fmt.Errorf("firmware too large (%d bytes, max %d)", info.Size(), maxFlashSize)
	}
	firmware, err := os.ReadFile(firmwarePath)
	if err != nil {
		return fmt.Errorf("reading firmware: %w", err)
	}
	return flashFirmwareBytes(portPath, firmware, transport, expectedChip, progressFn)
}

func flashFirmwareImage(portPath string, img *EspFlashImage, transport discovery.SerialTransport, expectedChip chipModel, progressFn func(pct float64)) error {
	return flashFirmwareBytes(portPath, img.Bytes(), transport, expectedChip, progressFn)
}

func flashFirmwareBytes(portPath string, firmware []byte, transport discovery.SerialTransport, expectedChip chipModel, progressFn func(pct float64)) error {
	if len(firmware) > maxFlashSize {
		return fmt.Errorf("firmware too large (%d bytes, max %d)", len(firmware), maxFlashSize)
	}

	mode := &serial.Mode{
		BaudRate: initialBaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}

	// Step 1: Enter bootloader and verify that it responds.
	f, err := connectAttempt(portPath, mode, transport)
	if err != nil {
		return err
	}
	defer func() { f.port.Close() }()

	// Step 2: Increase baud rate.
	if err := f.changeBaudRate(flashBaudRate); err != nil {
		return fmt.Errorf("change baud: %w", err)
	}

	// Step 3: Identify chip and disable watchdogs.
	if err := f.detectChip(); err != nil {
		return fmt.Errorf("detect chip: %w", err)
	}
	if err := validateDetectedChip(expectedChip, f.chip); err != nil {
		return err
	}
	if err := f.initChip(); err != nil {
		return fmt.Errorf("init chip: %w", err)
	}

	// Step 4: Attach SPI flash.
	if err := f.spiAttach(); err != nil {
		return fmt.Errorf("SPI attach: %w", err)
	}

	// Step 5: Reset flash chip and retrieve its JEDEC ID.
	jedecId, err := f.initFlashChip()
	if err != nil {
		return fmt.Errorf("init flash chip: %w", err)
	}

	// Step 6: Set SPI params.
	detectedFlashSize := flashSize(jedecId)
	dbgf("flash: JEDEC manufacturer=0x%02x type=0x%02x capacity=0x%02x size=%dKiB",
		jedecId.manufacturer, jedecId.memoryType, jedecId.capacity, detectedFlashSize/1024)
	if err := f.spiSetParams(detectedFlashSize); err != nil {
		return fmt.Errorf("SPI set params: %w", err)
	}

	// Step 7: Pre-flash eFuse/security checks.
	// Again, something done just to stick to the classic bootloader sequence.
	if err := f.preFlashChecks(); err != nil {
		return fmt.Errorf("pre-flash checks: %w", err)
	}

	// Step 8: Flash the firmware.
	totalSize := len(firmware)
	if totalSize > int(detectedFlashSize) {
		return fmt.Errorf("firmware too large (%d bytes, max %d)", totalSize, detectedFlashSize)
	}
	blockCount := (totalSize + espFlashBlockSize - 1) / espFlashBlockSize
	dbgf("flash: totalSize=%d blockCount=%d", totalSize, blockCount)
	if err := f.flashBegin(uint32(totalSize), uint32(blockCount), espFlashBlockSize, 0); err != nil {
		return fmt.Errorf("flash begin: %w", err)
	}

	for seq := uint32(0); seq < uint32(blockCount); seq++ {
		offset := int(seq) * espFlashBlockSize
		end := offset + espFlashBlockSize
		if end > len(firmware) {
			end = len(firmware)
		}

		block := make([]byte, espFlashBlockSize)
		// Fill with 0xFF (erased flash value) first, then copy actual data.
		for i := range block {
			block[i] = 0xFF
		}
		copy(block, firmware[offset:end])

		if err := f.flashData(block, seq); err != nil {
			return fmt.Errorf("flash block %d: %w", seq, err)
		}

		if progressFn != nil {
			progressFn(float64(seq+1) / float64(blockCount))
		}
	}

	// Mosaico uses USB OTG. Its ROM needs a watchdog reset to boot the app;
	// toggling DTR/RTS only applies to the USB Serial/JTAG controller.
	if f.chip == chipESP32S31 {
		return f.watchdogReset()
	}

	// Step 9: Reboot.
	// Please note that we never succeeded in using flashEnd() here.
	if err := resetESP32(f.port, false, transport); err != nil {
		return fmt.Errorf("resetting after flash: %w", err)
	}

	return nil
}

// watchdogReset follows the S31 ROM reset sequence used by esptool.
func (f *espFlasher) watchdogReset() error {
	for _, write := range [][2]uint32{
		{f.regs.wdtProtect, 0x50d83aa1},
		{f.regs.wdtConfig0 + 4, 2000},
		{f.regs.wdtConfig0, (1 << 31) | (5 << 28) | (1 << 8) | 2},
		{f.regs.wdtProtect, 0},
	} {
		if err := f.writeReg(write[0], write[1], 0xffffffff, 0); err != nil {
			return fmt.Errorf("S31 watchdog reset: %w", err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}
