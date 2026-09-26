package ipcam

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
)

// The MCU camera band must sit inside EnsureNode's guard and the kernel's
// device numbers, below the ROS 2 and IP bands.
func TestMCUBandBounds(t *testing.T) {
	if MCUBandEnd < MCUBandStart {
		t.Fatalf("MCU band end %d before start %d", MCUBandEnd, MCUBandStart)
	}
	// The sweep only reaps below LoopbackBandStart, so the MCU band is safe.
	if MCUBandStart < LoopbackBandStart || MCUBandEnd > LoopbackBandEnd {
		t.Fatalf("MCU band [%d,%d] outside the loopback band [%d,%d]", MCUBandStart, MCUBandEnd, LoopbackBandStart, LoopbackBandEnd)
	}
	if MCUBandEnd >= ros2camera.IDBandStart {
		t.Fatalf("MCU band ending at %d overlaps the ROS 2 band starting at %d", MCUBandEnd, ros2camera.IDBandStart)
	}
	if MCUBandEnd >= IDBandStart {
		t.Fatalf("MCU band ending at %d overlaps the IP band starting at %d", MCUBandEnd, IDBandStart)
	}
	// VIDEO_NUM_DEVICES is 256: the kernel renumbers any node asked for above.
	if LoopbackBandEnd > 255 {
		t.Fatalf("loopback band ends at %d, past the kernel's last device number 255", LoopbackBandEnd)
	}
}
