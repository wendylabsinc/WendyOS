//go:build linux

package ipcam

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The real v4l2loopback control surface, Linux only.
//
// Everything here is pinned to v4l2loopback 0.15.x's ABI — the module version
// the WendyOS wendy-v4l2loopback recipe builds (kernel 6.8; recipe pinned at
// 0.15.4, commit 0f9ee86760b7f2bea174b7e3e7a1d38845da0ab4; see WDY-2430 task
// F3's build report, "ABI record" section). 0.15.0 changed the module's
// public ioctl numbers from the pre-0.13 layout (0.13+ added
// min_width/min_height and moved announce_all_caps), so this struct and these
// constants are NOT portable to an older module build. If the recipe's pinned
// SRCREV ever moves, re-derive both from the new v4l2loopback.h before
// touching anything here — do not assume the layout is unchanged.

const (
	loopbackControlDevice = "/dev/v4l2loopback"

	// loopbackCardLabelMax is the usable length of card_label: the struct
	// field is char[32], and the driver treats it as a NUL-terminated C
	// string, so at most 31 bytes of a label survive, plus the terminator.
	loopbackCardLabelMax = 31
)

// v4l2LoopbackConfig mirrors v0.15.4's `struct v4l2_loopback_config` from
// v4l2loopback.h field-for-field, verified against the module's own fetched
// source (task F3's report quotes it directly, not paraphrased from docs):
//
//	struct v4l2_loopback_config {
//		__s32 output_nr;
//		__s32 unused;          /* capture_nr placeholder; not implemented, do not reuse */
//		char  card_label[32];
//		__u32 min_width;
//		__u32 max_width;
//		__u32 min_height;
//		__u32 max_height;
//		__s32 max_buffers;
//		__s32 max_openers;
//		__s32 debug;
//		__s32 announce_all_caps;
//	};
//
// Every field is 4-byte and 4-byte-aligned (including the char[32], which
// starts and ends on a 4-byte boundary), so there is no compiler padding
// anywhere in it — the Go struct below has the same 72-byte layout on both
// amd64 and arm64 (Jetson) with no manual alignment tricks required.
type v4l2LoopbackConfig struct {
	outputNr  int32
	unused    int32 // capture_nr placeholder — module does not implement split devices yet; never repurpose
	cardLabel [32]byte

	minWidth  uint32
	maxWidth  uint32
	minHeight uint32
	maxHeight uint32

	maxBuffers      int32
	maxOpeners      int32
	debug           int32
	announceAllCaps int32
}

// Control ioctl numbers for v4l2loopback 0.15.x, computed from the module's
// own macros in v4l2loopback.h:
//
//	#define V4L2LOOPBACK_CTL_IOCTLMAGIC '~'                                              // 0x7e
//	#define V4L2LOOPBACK_CTL_ADD    _IOW(V4L2LOOPBACK_CTL_IOCTLMAGIC, 1, struct v4l2_loopback_config)
//	#define V4L2LOOPBACK_CTL_REMOVE _IOW(V4L2LOOPBACK_CTL_IOCTLMAGIC, 2, __u32)
//
// expanded by hand against Linux's asm-generic ioctl encoding (the scheme
// every architecture this agent targets uses — x86_64 and arm64 both take the
// generic asm-generic/ioctl.h layout, not one of the MIPS/PowerPC/SPARC
// direction-bit variants):
//
//	_IOC_NRBITS=8 _IOC_TYPEBITS=8 _IOC_SIZEBITS=14 _IOC_DIRBITS=2
//	_IOC_NRSHIFT=0 _IOC_TYPESHIFT=8 _IOC_SIZESHIFT=16 _IOC_DIRSHIFT=30
//	_IOC_WRITE=1
//	_IOC(dir,type,nr,size) = dir<<30 | type<<8 | nr | size<<16
//
//	sizeof(struct v4l2_loopback_config) == 72 (no padding — see above)
//	V4L2LOOPBACK_CTL_ADD    = _IOC(1, 0x7e, 1, 72) = 1<<30 | 0x7e<<8 | 1 | 72<<16 = 0x40487E01
//	V4L2LOOPBACK_CTL_REMOVE = _IOC(1, 0x7e, 2,  4) = 1<<30 | 0x7e<<8 | 2 |  4<<16 = 0x40047E02
//
// These are NOT the legacy 0x4C80/0x4C81-style numbers that appear in some
// older v4l2loopback discussions — those belong to a pre-_IOW-macro module
// version this build does not use, with a differently-shaped config struct.
// Do not substitute them; recompute from the macros above if the pinned
// module version ever changes.
const (
	v4l2LoopbackCtlAdd    = 0x40487E01
	v4l2LoopbackCtlRemove = 0x40047E02
)

func defaultLoopbackDeps() loopbackDeps {
	return loopbackDeps{
		statControl: statLoopbackControl,
		modprobe:    modprobeLoopback,
		addNode:     addLoopbackNode,
		removeNode:  removeLoopbackNode,
		nodeExists:  loopbackNodeExists,
	}
}

func statLoopbackControl() error {
	_, err := os.Stat(loopbackControlDevice)
	return err
}

func loopbackNodeExists(nr int) bool {
	_, err := os.Stat(fmt.Sprintf("/dev/video%d", nr))
	return err == nil
}

// modprobeLoopback loads v4l2loopback with devices=0 (create no nodes
// automatically; EnsureNodes creates every node explicitly, numbered by
// camera ID) and exclusive_caps=1 (each node is OUTPUT-only to our pump and
// CAPTURE-only to the container, matching announce_all_caps=0 on every node
// this package adds). If the running module build rejects those parameters,
// it retries a plain load; Loopback.detect's sweepAutoCreatedNodes then cleans
// up whatever a plain load auto-creates below the reserved camera-ID band.
//
// Precedent for shelling out to modprobe: dhcpsock_linux.go's
// addLinkAddress/delLinkAddress, which shell out to `ip` the same way.
func modprobeLoopback(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "modprobe", "v4l2loopback", "devices=0", "exclusive_caps=1").CombinedOutput()
	if err == nil {
		return nil
	}
	if !strings.Contains(strings.ToLower(string(out)), "invalid") {
		return fmt.Errorf("modprobe v4l2loopback devices=0 exclusive_caps=1: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	out, err = exec.CommandContext(ctx, "modprobe", "v4l2loopback").CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe v4l2loopback (plain fallback after param rejection): %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// openLoopbackControl opens the control device fresh for each ioctl: this is
// not a hot path, and holding no long-lived handle means a module unload
// cannot leave Loopback pinning a stale file descriptor.
func openLoopbackControl() (*os.File, error) {
	fd, err := unix.Open(loopbackControlDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", loopbackControlDevice, err)
	}
	return os.NewFile(uintptr(fd), loopbackControlDevice), nil
}

// addLoopbackNode issues V4L2LOOPBACK_CTL_ADD for an explicit node number.
// EEXIST at nr is treated as success — the struct's own doc comment says
// requesting an nr that already exists returns EEXIST, and an existing node
// is exactly the state EnsureNodes wants. The driver returning a different nr
// than requested should not be possible for an explicit (>=0) nr per that
// same doc comment, but a stray node is expensive to leave behind, so it is
// removed defensively rather than assumed away.
func addLoopbackNode(nr int, label string) error {
	f, err := openLoopbackControl()
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	cfg := v4l2LoopbackConfig{
		outputNr:   int32(nr),
		maxOpeners: 8,
		// announceAllCaps left 0: exclusive-caps semantics, matching the
		// exclusive_caps=1 module parameter modprobeLoopback requests.
	}
	setCardLabel(&cfg, label)

	ret, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(v4l2LoopbackCtlAdd), uintptr(unsafe.Pointer(&cfg)))
	if errno != 0 {
		if errno == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("V4L2LOOPBACK_CTL_ADD nr=%d: %w", nr, errno)
	}
	if int(ret) != nr {
		_ = removeLoopbackNode(int(ret))
		return fmt.Errorf("V4L2LOOPBACK_CTL_ADD nr=%d: kernel created nr=%d instead; removed it", nr, int(ret))
	}
	return nil
}

// removeLoopbackNode issues V4L2LOOPBACK_CTL_REMOVE. Unlike ADD, it takes the
// device number itself as the ioctl argument, not a pointer to it: the
// module's control handler reads the argument as the number. A pointer is
// read as a device number too, which matches nothing, so the kernel answers
// ENODEV and the node stays (seen on v4l2loopback 0.15.4 on a Jetson).
// A target that no longer exists (ENODEV) is treated as success: RemoveCamera
// is best-effort, and a node already gone is the desired end state, not a
// failure.
func removeLoopbackNode(nr int) error {
	f, err := openLoopbackControl()
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(v4l2LoopbackCtlRemove), uintptr(uint32(nr)))
	if errno != 0 {
		if errno == unix.ENODEV {
			return nil
		}
		return fmt.Errorf("V4L2LOOPBACK_CTL_REMOVE nr=%d: %w", nr, errno)
	}
	return nil
}

// setCardLabel copies label into cfg.cardLabel, truncated to leave room for
// the NUL terminator the driver expects. cfg.cardLabel starts zero-valued, so
// no explicit terminator write is needed.
func setCardLabel(cfg *v4l2LoopbackConfig, label string) {
	b := []byte(label)
	if len(b) > loopbackCardLabelMax {
		b = b[:loopbackCardLabelMax]
	}
	copy(cfg.cardLabel[:], b)
}
