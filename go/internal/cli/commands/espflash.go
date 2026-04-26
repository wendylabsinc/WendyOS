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
	espCmdFlashBegin   = 0x02
	espCmdFlashData    = 0x03
	espCmdFlashEnd     = 0x04
	espCmdSync         = 0x08
	espCmdSPISetParams = 0x0B
	espCmdSPIAttach    = 0x0D
	espCmdChangeBaud   = 0x0F
)

// SLIP framing bytes.
const (
	slipEnd    = 0xC0
	slipEsc    = 0xDB
	slipEscEnd = 0xDC
	slipEscEsc = 0xDD
)

const (
	espFlashBlockSize = 0x4000 // 16 KiB per flash data block
	espSyncTimeout    = 3 * time.Second
	espCmdTimeout     = 10 * time.Second
	flashBaudRate     = 921600
	initialBaudRate   = 115200
)

// espFlasher handles serial communication with the ESP32 bootloader.
type espFlasher struct {
	port serial.Port
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

	if _, err := f.port.Write(encoded); err != nil {
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

		// Bytes 2:4 = size, 4:8 = value/error.
		// Check the status field: last 4 bytes of the 8-byte header encode
		// the return value. For most commands, a non-zero "error" field at
		// byte offset 8+size-1 (the status struct appended by ROM loader)
		// indicates failure. We return the payload and let callers check.
		return resp[8:], nil
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

// spiAttach attaches the SPI flash.
func (f *espFlasher) spiAttach() error {
	// ESP32-C6 ROM bootloader expects 4 bytes (SPI config = 0 for defaults).
	data := make([]byte, 4)
	f.port.SetReadTimeout(espCmdTimeout)
	_, err := f.sendCommand(espCmdSPIAttach, data, 0)
	return err
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
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data[0:4], size)
	binary.LittleEndian.PutUint32(data[4:8], blockCount)
	binary.LittleEndian.PutUint32(data[8:12], blockSize)
	binary.LittleEndian.PutUint32(data[12:16], offset)

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
	defer port.Close()

	f := &espFlasher{port: port}

	// Enter bootloader: toggle DTR/RTS to reset into download mode.
	// Sequence: assert RTS (EN low) → assert DTR (IO0 low) → release RTS → release DTR.
	port.SetDTR(false)
	port.SetRTS(true)
	time.Sleep(100 * time.Millisecond)
	port.SetDTR(true)
	port.SetRTS(false)
	time.Sleep(100 * time.Millisecond)
	port.SetDTR(false)
	time.Sleep(50 * time.Millisecond)

	// Drain any boot output.
	f.drain()

	// Step 1: Sync.
	if err := f.sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	// Step 2: Increase baud rate.
	if err := f.changeBaudRate(flashBaudRate); err != nil {
		return fmt.Errorf("change baud: %w", err)
	}

	// Step 3: Attach SPI flash.
	if err := f.spiAttach(); err != nil {
		return fmt.Errorf("SPI attach: %w", err)
	}

	// Step 4: Set SPI params (4 MB flash).
	if err := f.spiSetParams(4 * 1024 * 1024); err != nil {
		return fmt.Errorf("SPI set params: %w", err)
	}

	// Step 5: Flash the firmware.
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

	// Step 6: Finish and reboot.
	if err := f.flashEnd(true); err != nil {
		return fmt.Errorf("flash end: %w", err)
	}

	return nil
}
