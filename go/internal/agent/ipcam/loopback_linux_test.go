//go:build linux

package ipcam

import (
	"os"
	"testing"
)

// TestLoopbackNodeRemoveDeletesTheNode adds a real node and removes it. It
// needs root and the v4l2loopback module, so it skips anywhere else; run it on
// a device. A removal that reports success but leaves the node behind leaks a
// number from the camera band until the device reboots.
func TestLoopbackNodeRemoveDeletesTheNode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if err := statLoopbackControl(); err != nil {
		t.Skipf("no v4l2loopback control device: %v", err)
	}
	// Below the ROS 2 and network camera bands, and above any real camera.
	nr := -1
	for n := 127; n >= 100; n-- {
		if !loopbackNodeExists(n) {
			nr = n
			break
		}
	}
	if nr < 0 {
		t.Skip("no free node number in 100-127")
	}
	if err := addLoopbackNode(nr, "wendy-remove-test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeLoopbackNode(nr) })
	if !loopbackNodeExists(nr) {
		t.Fatalf("/dev/video%d is missing after it was added", nr)
	}
	if err := removeLoopbackNode(nr); err != nil {
		t.Fatalf("removing /dev/video%d: %v", nr, err)
	}
	if loopbackNodeExists(nr) {
		t.Fatalf("/dev/video%d still exists after removeLoopbackNode reported success", nr)
	}
}

func TestNewLoopbackConfig_AnnounceAllCapsOnlyForShareNodes(t *testing.T) {
	if cfg := newLoopbackConfig(5, "cam", 0); cfg.announceAllCaps != 0 || cfg.outputNr != 5 || cfg.maxOpeners != 8 {
		t.Fatalf("camera node config = %+v", cfg)
	}
	if cfg := newLoopbackConfig(5, "share", 1); cfg.announceAllCaps != 1 || cfg.maxOpeners != 8 {
		t.Fatalf("share node config = %+v", cfg)
	}
}
