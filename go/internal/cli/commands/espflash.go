//go:build darwin || linux || windows

package commands

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"

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

// ESP32-C6 register addresses used during flash initialisation.
const (
	regChipMagic = 0x4087f580 // chip magic/identification word
	regChipID0   = 0x600b0850 // eFuse chip ID word 0
	regChipID1   = 0x600b0854 // eFuse chip ID word 1
	regMACLow    = 0x600b0844 // eFuse MAC address low word
	regMACHigh   = 0x600b0848 // eFuse MAC address high word
	regEfuseA    = 0x600b0830 // eFuse miscellaneous register A
	regEfuseB    = 0x600b0838 // eFuse miscellaneous register B
	regRTCCtrl00 = 0x600b1c00 // RTC control register 0x00
	regRTCCtrl18 = 0x600b1c18 // RTC control register 0x18
	regRTCCtrl1C = 0x600b1c1c // RTC control register 0x1c
	regRTCCtrl20 = 0x600b1c20 // RTC control register 0x20
	regSPICmd    = 0x60003000 // SPI_MEM_CMD_REG
	regSPIUser   = 0x60003018 // SPI_MEM_USER_REG
	regSPIUser1  = 0x60003020 // SPI_MEM_USER1_REG
	regSPIClock  = 0x60003028 // SPI_MEM_CLOCK_REG
	regSPIW0     = 0x60003058 // SPI_MEM_W0_REG (data buffer word 0)
)

// SLIP framing bytes.
const (
	slipEnd    = 0xC0
	slipEsc    = 0xDB
	slipEscEnd = 0xDC
	slipEscEsc = 0xDD
)

const (
	espFlashBlockSize = 0x1000 // 4 KiB per flash data block
	espSyncTimeout    = 3 * time.Second
	espCmdTimeout     = 10 * time.Second
	flashBaudRate     = 921600
	initialBaudRate   = 115200
)

// JedecID holds the three-byte JEDEC flash identification returned by the
// RDID (0x9f) command.
type JedecID struct {
	manufacturer byte // vendor code (e.g. 0xEF = Winbond, 0x20 = Micron)
	memoryType   byte // memory technology and interface (e.g. 0x40 = SPI NOR)
	capacity     byte // density code (e.g. 0x17 = 64 Mbit)
}

// espFlasher handles serial communication with the ESP32 bootloader.
type espFlasher struct {
	port serial.Port
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
	return uint32(1) << id.capacity
}

// slipEncode wraps data in SLIP framing.
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
	deadline := time.Now().Add(espCmdTimeout)
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

// slipDecode reads the next non-empty SLIP frame from the port.
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
				// End of frame. Skip empty frames (consecutive 0xC0 bytes).
				if len(frame) > 0 {
					return frame, nil
				}
				// Empty frame — the outer loop will look for the next one.
				// But this 0xC0 could itself be the start of the next frame,
				// so break out of the inner loop and fall through.
				goto nextFrame
			case slipEsc:
				escaped = true
			default:
				frame = append(frame, b)
			}
		}
	nextFrame:
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

// drain discards any pending data in the serial receive buffer.
func (f *espFlasher) drain() {
	f.port.SetReadTimeout(50 * time.Millisecond)
	buf := make([]byte, 512)
	for {
		n, _ := f.port.Read(buf)
		if n == 0 {
			break
		}
	}
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
		f.port.SetReadTimeout(espSyncTimeout)
		_, err := f.sendCommand(espCmdSync, data, 0)
		if err == nil {
			// Drain extra sync responses (bootloader sends multiple).
			f.drain()
			f.port.SetReadTimeout(espCmdTimeout)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("failed to sync with ESP32 bootloader after 10 attempts")
}

// changeBaudRate switches the bootloader to a faster baud rate.
func (f *espFlasher) changeBaudRate(newBaud int) error {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[0:4], uint32(newBaud))
	binary.LittleEndian.PutUint32(data[4:8], uint32(initialBaudRate))

	f.port.SetReadTimeout(espCmdTimeout)
	if _, err := f.sendCommand(espCmdChangeBaud, data, 0); err != nil {
		return fmt.Errorf("changing baud rate: %w", err)
	}

	// Drain any data still at the old baud rate before switching.
	f.drain()

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
	f.drain()

	return nil
}

// getSecurityInfo queries the chip security info (opcode 0x14).
func (f *espFlasher) getSecurityInfo() (uint32, error) {
	f.port.SetReadTimeout(espCmdTimeout)
	resp, err := f.sendCommand(espCmdGetSecurityInfo, nil, 0)
	if err != nil {
		return 0, err
	}
	if len(resp) < 4 {
		return 0, fmt.Errorf("getSecurityInfo: response too short (%d bytes)", len(resp))
	}
	return binary.LittleEndian.Uint32(resp[:4]), nil
}

// readReg reads a 32-bit peripheral register at addr.
// The ROM bootloader returns the value in the header value field (bytes 4–7
// of the raw response), which sendCommand exposes as result[0:4].
func (f *espFlasher) readReg(addr uint32) (uint32, error) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, addr)
	f.port.SetReadTimeout(espCmdTimeout)
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
	f.port.SetReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdWriteReg, data, 0)
	return err
}

// spiAttach attaches the SPI flash.
func (f *espFlasher) spiAttach() error {
	data := make([]byte, 8)
	f.port.SetReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdSPIAttach, data, 0)
	return err
}

// chipDetect runs the register read/write sequence the ROM bootloader requires
// for chip identification.  We target ESP32-C6 only, so results are discarded.
func (f *espFlasher) chipDetect() error {
	if _, err := f.readReg(regChipMagic); err != nil {
		return err
	}

	// RTC / JTAG power-domain initialisation sequence observed in esptool trace.
	if err := f.writeReg(regRTCCtrl18, 0x50d83aa1, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(regRTCCtrl00, 0x00000000, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(regRTCCtrl18, 0x00000000, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(regRTCCtrl20, 0x50d83aa1, 0xffffffff, 0); err != nil {
		return err
	}

	// Read-modify-write: write back the value currently in regRTCCtrl1C.
	val, err := f.readReg(regRTCCtrl1C)
	if err != nil {
		return err
	}
	if err := f.writeReg(regRTCCtrl1C, val, 0xffffffff, 0); err != nil {
		return err
	}
	if err := f.writeReg(regRTCCtrl20, 0x00000000, 0xffffffff, 0); err != nil {
		return err
	}

	// Chip-ID reads (x3 for regChipID0, x1 for regChipID1).
	for i := 0; i < 3; i++ {
		if _, err := f.readReg(regChipID0); err != nil {
			return err
		}
	}
	if _, err := f.readReg(regChipID1); err != nil {
		return err
	}

	// MAC address reads (three sets of low+high words).
	for i := 0; i < 3; i++ {
		if _, err := f.readReg(regMACLow); err != nil {
			return err
		}
		if _, err := f.readReg(regMACHigh); err != nil {
			return err
		}
	}

	return nil
}

// initFlashChip performs the SPI flash controller register sequence
// observed in the esptool trace after SPI_ATTACH.
// It retrives the JEDEC ID and resets the flash chip, in order to start
// without depeding on previous usages.
func (f *espFlasher) initFlashChip() (JedecID, error) {
	user0, err := f.readReg(regSPIUser)
	if err != nil {
		return JedecID{}, err
	}
	user1, err := f.readReg(regSPIUser1)
	if err != nil {
		return JedecID{}, err
	}

	// Step 1: RDID (0x9f) — read JEDEC ID using a faster clock.
	if err := f.writeReg(regSPIClock, 0x00000017, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser, 0x90000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser1, 0x7000009f, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIW0, 0x00000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPICmd, 0x00040000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPICmd); err != nil { // poll until done
		return JedecID{}, err
	}
	// W0 layout: bits 7:0 = Manufacturer, bits 15:8 = MemoryType, bits 23:16 = Capacity.
	w0, err := f.readReg(regSPIW0)
	if err != nil {
		return JedecID{}, err
	}
	id := JedecID{
		manufacturer: byte(w0),
		memoryType:   byte(w0 >> 8),
		capacity:     byte(w0 >> 16),
	}
	// Restore and verify.
	if err := f.writeReg(regSPIUser, user0, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser1, user1, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPIUser); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPIUser1); err != nil {
		return JedecID{}, err
	}

	// Step 2: RSTEN (0x66) — Reset Enable command.
	if err := f.writeReg(regSPIUser, 0x80000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser1, 0x70000066, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIW0, 0x00000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPICmd, 0x00040000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPICmd); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPIW0); err != nil {
		return JedecID{}, err
	}
	// Restore and verify.
	if err := f.writeReg(regSPIUser, user0, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser1, user1, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPIUser); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPIUser1); err != nil {
		return JedecID{}, err
	}

	// Step 3: RST (0x99) — Reset command.
	if err := f.writeReg(regSPIUser, 0x80000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser1, 0x70000099, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIW0, 0x00000000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPICmd, 0x00040000, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPICmd); err != nil {
		return JedecID{}, err
	}
	if _, err := f.readReg(regSPIW0); err != nil {
		return JedecID{}, err
	}
	// Final restore (no verify needed after the last attempt).
	if err := f.writeReg(regSPIUser, user0, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}
	if err := f.writeReg(regSPIUser1, user1, 0xffffffff, 0); err != nil {
		return JedecID{}, err
	}

	return id, nil
}

// preFlashChecks performs the eFuse/security register reads observed on the esptool trace.
func (f *espFlasher) preFlashChecks() error {
	if _, err := f.readReg(regEfuseB); err != nil {
		return err
	}
	if _, err := f.readReg(regChipID0); err != nil {
		return err
	}
	if _, err := f.readReg(regChipID0); err != nil {
		return err
	}
	if _, err := f.readReg(regEfuseA); err != nil {
		return err
	}
	// Final check immediately before erase/flash-begin.
	if _, err := f.readReg(regEfuseB); err != nil {
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

	f.port.SetReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdSPISetParams, data, 0)
	return err
}

// flashBegin starts a flash write operation, erasing the target region.
func (f *espFlasher) flashBegin(size, blockCount, blockSize, offset uint32) error {
	data := make([]byte, 20)
	binary.LittleEndian.PutUint32(data[0:4], size)
	binary.LittleEndian.PutUint32(data[4:8], blockCount)
	binary.LittleEndian.PutUint32(data[8:12], blockSize)
	binary.LittleEndian.PutUint32(data[12:16], offset)
	binary.LittleEndian.PutUint32(data[16:20], 0) // 0 = no encryption

	f.port.SetReadTimeout(30 * time.Second) // erase can be slow
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

	f.port.SetReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdFlashData, data, checksum)
	return err
}

// flashEnd finishes the flash operation.
func (f *espFlasher) flashEnd(reboot bool) error {
	data := make([]byte, 4)
	if !reboot {
		binary.LittleEndian.PutUint32(data, 1) // 1 = don't reboot
	}

	f.port.SetReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdFlashEnd, data, 0)
	return err
}

// Reset the chip and eventually enter download mode.
// It uses the ESP32 USB-Serial/JTAG peripheral. ESP-IDF's USB-JTAG driver watches for this
// specific DTR/RTS pattern and triggers a software reset into download mode.
// This matches esptool's USBJTAGSerialReset strategy.
func espResetViaUsbJtag(port serial.Port, enterBootloader bool) {
	port.SetDTR(false)
	port.SetRTS(false)
	time.Sleep(100 * time.Millisecond)
	port.SetDTR(enterBootloader) // GPIO0=LOW (download mode selected)
	port.SetRTS(false)
	time.Sleep(100 * time.Millisecond)
	port.SetRTS(true) // EN=LOW (assert reset)
	port.SetDTR(false)
	time.Sleep(100 * time.Millisecond)
	port.SetRTS(false) // EN=HIGH (release reset → boots into download mode)
	time.Sleep(50 * time.Millisecond)
}

// flashFirmware is the main entry point: flash a .bin file to the ESP32.
func flashFirmware(portPath, firmwarePath string, progressFn func(pct float64)) error {
	firmware, err := os.ReadFile(firmwarePath)
	if err != nil {
		return fmt.Errorf("reading firmware: %w", err)
	}

	mode := &serial.Mode{
		BaudRate: initialBaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}

	port, err := serial.Open(portPath, mode)
	if err != nil {
		return fmt.Errorf("opening serial port %s: %w", portPath, err)
	}

	f := &espFlasher{port: port}
	defer func() { f.port.Close() }()

	// Step 1: Enter bootloader.
	espResetViaUsbJtag(port, true)
	f.port.Close()
	time.Sleep(1500 * time.Millisecond) // wait for USB re-enumeration
	newPort, err := serial.Open(portPath, mode)
	if err != nil {
		return fmt.Errorf("reopening port after reset: %w", err)
	}
	f.port = newPort
	f.drain()

	// Verify that the bootloader is responding
	if err := f.sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	// Step 2: Increase baud rate.
	if err := f.changeBaudRate(flashBaudRate); err != nil {
		return fmt.Errorf("change baud: %w", err)
	}

	// Step 3: Security info + chip detection register sequence.
	// The responses are ignored, but we perform them to stay close to the
	// classic ROM bootloader init sequence.
	if _, err := f.getSecurityInfo(); err != nil {
		return fmt.Errorf("get security info: %w", err)
	}
	if err := f.chipDetect(); err != nil {
		return fmt.Errorf("chip detect: %w", err)
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
	flashSize := flashSize(jedecId)
	if err := f.spiSetParams(flashSize); err != nil {
		return fmt.Errorf("SPI set params: %w", err)
	}

	// Step 7: Pre-flash eFuse/security checks.
	// Again, something done just to stick to the classic bootloader sequence.
	if err := f.preFlashChecks(); err != nil {
		return fmt.Errorf("pre-flash checks: %w", err)
	}

	// Step 8: Flash the firmware.
	totalSize := uint32(len(firmware))
	blockCount := (totalSize + espFlashBlockSize - 1) / espFlashBlockSize
	if err := f.flashBegin(totalSize, blockCount, espFlashBlockSize, 0); err != nil {
		return fmt.Errorf("flash begin: %w", err)
	}

	for seq := uint32(0); seq < blockCount; seq++ {
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

	// Step 6: Reboot.
	// Please note that we never succeeded in using flashEnd() here.
	espResetViaUsbJtag(port, false)

	return nil
}
