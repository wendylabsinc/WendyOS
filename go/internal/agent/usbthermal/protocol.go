package usbthermal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// The control protocol. Interface 0 (vendor class 0xFF/0xF0) takes 18-byte
// commands; interface 1 carries the stream on bulk IN 0x81 once alternate
// setting 1 is selected. Everything below was checked against the TC001 Max on
// exxon-01, and matches what the community documented for the same vendor's
// Thermal Master P3 (see the package comment for credits).
const (
	// bmRequestType values: vendor requests to the interface, or the device.
	reqTypeVendorOutInterface = 0x41
	reqTypeVendorInInterface  = 0xC1
	reqTypeVendorOutDevice    = 0x40

	// bRequest values.
	reqCommand  = 0x20 // OUT: one 18-byte command
	reqResponse = 0x21 // IN: the command's response
	reqStatus   = 0x22 // IN: 1 byte; bit 0 set while the camera is busy
	reqStream   = 0xEE // OUT to the device, wIndex 1, no data: start the stream

	controlInterface = 0
	streamInterface  = 1
	streamAltIdle    = 0
	streamAltOn      = 1
	streamEndpoint   = 0x81

	// bulkReadBytes is one usbfs bulk read. 16 KiB is the per-transfer limit
	// older kernels enforce, and plenty: a read every ~0.6 ms at the stream's
	// ~28 MB/s.
	bulkReadBytes = 16 * 1024

	controlTimeout = time.Second
	// readTimeout bounds one bulk read. The stream never pauses for this long
	// while it is running, so a timeout means it has stopped.
	readTimeout = time.Second

	// statusPolls bounds how long a command may stay busy: 100 polls 10 ms
	// apart.
	statusPolls        = 100
	statusPollInterval = 10 * time.Millisecond

	// The pauses the start sequence needs. Without the second one the camera
	// does not start streaming (community-documented for the P3, and the
	// sequence the TC001 Max was verified with).
	settleAfterStart     = time.Second
	settleAfterInterface = 2 * time.Second

	modelResponseBytes = 30
)

// Commands are 18 bytes:
//
//	0  byte       0x01
//	1  byte       command: 0x01 read register, 0x2f stream control
//	2  uint16 LE  parameter (0x0081 here)
//	4  uint16 LE  register
//	6  6 bytes    zero
//	12 uint16 LE  response length
//	14 2 bytes    zero
//	16 uint16 LE  CRC-16/XMODEM (CCITT, polynomial 0x1021, initial 0) of bytes 0..15
//
// The camera is said not to check the CRC; it is computed anyway, and the two
// commands the driver sends are byte-for-byte the ones verified on hardware
// (TestCommandsMatchTheVerifiedBytes).
var (
	cmdReadModel   = command(0x01, 0x0081, 0x0001, modelResponseBytes)
	cmdStartStream = command(0x2f, 0x0081, 0x0000, 1)
)

func command(cmd byte, param, register, responseLen uint16) []byte {
	b := make([]byte, 18)
	b[0], b[1] = 0x01, cmd
	binary.LittleEndian.PutUint16(b[2:], param)
	binary.LittleEndian.PutUint16(b[4:], register)
	binary.LittleEndian.PutUint16(b[12:], responseLen)
	binary.LittleEndian.PutUint16(b[16:], crc16CCITT(b[:16]))
	return b
}

func crc16CCITT(data []byte) uint16 {
	var crc uint16
	for _, v := range data {
		crc ^= uint16(v) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// sleepFunc pauses for d or until ctx ends. A seam so tests do not wait out
// the start sequence's three seconds.
type sleepFunc func(ctx context.Context, d time.Duration) error

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// session is one opened camera with its interfaces claimed.
type session struct {
	dev     Device
	sleep   sleepFunc
	claimed []uint32
	// streaming records that interface 1 left alternate setting 0, so stop
	// knows to put it back.
	streaming bool
}

// sendCommand runs one command: write it, wait while the camera reports busy,
// read the response, read the status once more (the vendor tool does, and the
// community found it makes the camera more reliable).
func (s *session) sendCommand(ctx context.Context, cmd []byte, responseLen int) ([]byte, error) {
	if _, err := s.dev.Control(reqTypeVendorOutInterface, reqCommand, 0, controlInterface, cmd, controlTimeout); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}
	status := make([]byte, 1)
	for poll := 0; ; poll++ {
		if _, err := s.dev.Control(reqTypeVendorInInterface, reqStatus, 0, controlInterface, status, controlTimeout); err != nil {
			return nil, fmt.Errorf("reading command status: %w", err)
		}
		if status[0]&1 == 0 {
			break
		}
		if poll >= statusPolls {
			return nil, fmt.Errorf("camera stayed busy after a command (status %#02x)", status[0])
		}
		if err := s.sleep(ctx, statusPollInterval); err != nil {
			return nil, err
		}
	}
	var resp []byte
	if responseLen > 0 {
		resp = make([]byte, responseLen)
		n, err := s.dev.Control(reqTypeVendorInInterface, reqResponse, 0, controlInterface, resp, controlTimeout)
		if err != nil {
			return nil, fmt.Errorf("reading command response: %w", err)
		}
		resp = resp[:n]
	}
	if _, err := s.dev.Control(reqTypeVendorInInterface, reqStatus, 0, controlInterface, status, controlTimeout); err != nil {
		return nil, fmt.Errorf("reading command status: %w", err)
	}
	return resp, nil
}

// readModel asks the camera for its model string (register 0x01).
func (s *session) readModel(ctx context.Context) (string, error) {
	resp, err := s.sendCommand(ctx, cmdReadModel, modelResponseBytes)
	if err != nil {
		return "", err
	}
	if i := bytes.IndexByte(resp, 0); i >= 0 {
		resp = resp[:i]
	}
	return string(resp), nil
}

// start claims both interfaces and starts the stream. The sequence is the one
// verified on the TC001 Max:
//
//  1. claim interfaces 0 and 1;
//  2. read the model register (diagnostics, and a first proof the camera
//     answers);
//  3. start_stream, then wait 1 s;
//  4. interface 1 to alternate setting 1, then vendor request 0xEE to the
//     device with wIndex 1;
//  5. wait 2 s, then one bulk read whose bytes are discarded;
//  6. start_stream again.
//
// On error the caller must still call stop: whatever was claimed or switched
// on is undone there.
func (s *session) start(ctx context.Context) (model string, err error) {
	for _, iface := range []uint32{controlInterface, streamInterface} {
		if err := s.dev.ClaimInterface(iface); err != nil {
			return "", fmt.Errorf("claiming interface %d: %w", iface, err)
		}
		s.claimed = append(s.claimed, iface)
	}
	model, err = s.readModel(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the camera model: %w", err)
	}
	if _, err := s.sendCommand(ctx, cmdStartStream, 1); err != nil {
		return model, fmt.Errorf("start_stream: %w", err)
	}
	if err := s.sleep(ctx, settleAfterStart); err != nil {
		return model, err
	}
	if err := s.dev.SetInterface(streamInterface, streamAltOn); err != nil {
		return model, fmt.Errorf("selecting the streaming interface: %w", err)
	}
	s.streaming = true
	if _, err := s.dev.Control(reqTypeVendorOutDevice, reqStream, 0, streamInterface, nil, controlTimeout); err != nil {
		return model, fmt.Errorf("enabling the stream: %w", err)
	}
	if err := s.sleep(ctx, settleAfterInterface); err != nil {
		return model, err
	}
	// The vendor tool issues one read here and the stream does not start
	// reliably without it. What it returns (often nothing) is not frame data
	// the parser could use, and a timeout is normal.
	prime := make([]byte, bulkReadBytes)
	if _, err := s.dev.Bulk(streamEndpoint, prime, readTimeout); err != nil && !errors.Is(err, ErrTimeout) {
		return model, fmt.Errorf("priming the stream: %w", err)
	}
	if _, err := s.sendCommand(ctx, cmdStartStream, 1); err != nil {
		return model, fmt.Errorf("start_stream: %w", err)
	}
	return model, nil
}

// stop puts the camera back the way the agent found it: interface 1 at
// alternate setting 0 (which is what stops the stream) and both interfaces
// released. It is best-effort and runs every step even when one fails, since a
// camera that was unplugged fails all of them.
func (s *session) stop() error {
	var errs []error
	if s.streaming {
		if err := s.dev.SetInterface(streamInterface, streamAltIdle); err != nil {
			errs = append(errs, fmt.Errorf("stopping the stream: %w", err))
		}
		s.streaming = false
	}
	for i := len(s.claimed) - 1; i >= 0; i-- {
		if err := s.dev.ReleaseInterface(s.claimed[i]); err != nil {
			errs = append(errs, fmt.Errorf("releasing interface %d: %w", s.claimed[i], err))
		}
	}
	s.claimed = nil
	return errors.Join(errs...)
}
