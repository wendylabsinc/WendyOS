package realsense

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	uvciocCtrlQuery = 0xc0107521 // UVCIOC_CTRL_QUERY: _IOWR('u', 0x21, struct uvc_xu_control_query)
	uvcSetCur       = 0x01
	uvcGetCur       = 0x81
	uvcGetLen       = 0x85

	xuUnit     = 3 // DS5 depth extension unit bUnitID
	xuSelector = 1 // DS5_HWMONITOR
)

// uvcXUControlQuery mirrors struct uvc_xu_control_query (uvcvideo.h).
type uvcXUControlQuery struct {
	unit     uint8
	selector uint8
	query    uint8
	_        uint8
	size     uint16
	_        [2]byte
	data     *byte
}

// nodeTransport is the live Transport: HWM frames over the depth node's
// extension unit. It holds the fd for the duration of the calibration read
// only — control queries coexist with streaming, so holding it briefly never
// contends with a capture.
type nodeTransport struct {
	fd int
}

// OpenTransport opens the depth node and verifies it exposes the DS5 hardware
// monitor (control length exactly 1024). Anything else — a colour node, a
// non-RealSense camera — fails here, before any command is sent.
func OpenTransport(devPath string) (*nodeTransport, error) {
	fd, err := unix.Open(devPath, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("realsense: open %s: %w", devPath, err)
	}
	t := &nodeTransport{fd: fd}
	lenBuf := make([]byte, 2)
	if err := t.ioctlXU(uvcGetLen, lenBuf); err != nil {
		t.Close()
		return nil, fmt.Errorf("realsense: %s has no DS5 hardware monitor: %w", devPath, err)
	}
	if l := binary.LittleEndian.Uint16(lenBuf); l != hwmFrameSize {
		t.Close()
		return nil, fmt.Errorf("realsense: %s HWM control length is %d, want %d", devPath, l, hwmFrameSize)
	}
	return t, nil
}

func (t *nodeTransport) Close() error { return unix.Close(t.fd) }

// ioctlXU issues one UVC extension-unit query. The kernel requires the size
// to match the control's GET_LEN exactly (else -ENOBUFS). The retry here IS
// the whole budget: at most three extra attempts on the transient errnos a
// busy UVC function returns, then fail — never loop. A retry storm on this
// control path once wedged a Jetson's xHCI controller.
func (t *nodeTransport) ioctlXU(query uint8, buf []byte) error {
	q := uvcXUControlQuery{
		unit:     xuUnit,
		selector: xuSelector,
		query:    query,
		size:     uint16(len(buf)),
		data:     &buf[0],
	}
	for attempt := 0; ; attempt++ {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(t.fd), uvciocCtrlQuery, uintptr(unsafe.Pointer(&q)))
		if errno == 0 {
			return nil
		}
		if attempt < 3 && (errno == unix.EAGAIN || errno == unix.EBUSY || errno == unix.EIO) {
			continue
		}
		return fmt.Errorf("UVCIOC_CTRL_QUERY query=0x%02x size=%d: %w", query, len(buf), errno)
	}
}

// Exchange implements Transport: SET_CUR the command frame, GET_CUR the reply.
func (t *nodeTransport) Exchange(tx []byte) ([]byte, error) {
	if len(tx) != hwmFrameSize {
		return nil, fmt.Errorf("realsense: command frame is %d bytes, want %d", len(tx), hwmFrameSize)
	}
	if err := t.ioctlXU(uvcSetCur, tx); err != nil {
		return nil, fmt.Errorf("SET_CUR: %w", err)
	}
	rx := make([]byte, hwmFrameSize)
	if err := t.ioctlXU(uvcGetCur, rx); err != nil {
		return nil, fmt.Errorf("GET_CUR: %w", err)
	}
	return rx, nil
}
