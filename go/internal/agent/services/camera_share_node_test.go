package services

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// blankFrames caches blankFrame by mode.
var blankFrames sync.Map // sharedMode -> []byte

// blankFrame is one black frame in mode. The slice is shared and cached:
// callers must not modify it.
func blankFrame(mode sharedMode) []byte {
	if v, ok := blankFrames.Load(mode); ok {
		return v.([]byte)
	}
	frame := make([]byte, mode.frameBytes())
	var pattern []byte
	switch mode.Fourcc {
	case "YUYV":
		pattern = []byte{0x10, 0x80, 0x10, 0x80}
	case "UYVY":
		pattern = []byte{0x80, 0x10, 0x80, 0x10}
	}
	for i := range pattern {
		for j := i; j < len(frame); j += len(pattern) {
			frame[j] = pattern[i]
		}
	}
	v, _ := blankFrames.LoadOrStore(mode, frame)
	return v.([]byte)
}

func TestShareNodeIoctlNumbers(t *testing.T) {
	if vidiocSCtrl != 0xC008561C || v4l2LoopbackCidKeepFormat != 0x0098f900 || v4l2LoopbackCidFreshStart != 0x0098f904 || unsafe.Sizeof(v4l2Control{}) != 8 {
		t.Fatal("keep_format ioctl constants drifted")
	}
	if vidiocSubscribeEvent != 0x4020565A || vidiocDqevent != 0x80885659 ||
		v4l2EventClientUsage != 0x10E00001 || v4l2EventSubFlSendInitial != 1 || shareNodeBuffers != 2 {
		t.Fatal("share node constants drifted from the values Proof 1 measured")
	}
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func TestShareNodeWriteFrameRefusesWrongSize(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "YUYV", BytesPerLine: 8, FPS: 30}
	w := &shareNodeWriter{mode: mode, fd: -1, wakeR: -1, wakeW: -1, done: closedChan()}
	if err := w.WriteFrame(make([]byte, mode.frameBytes()-1), 1); err == nil {
		t.Fatal("a short frame was accepted")
	}
	if err := w.WriteFrame(make([]byte, mode.frameBytes()+1), 1); err == nil {
		t.Fatal("a long frame was accepted")
	}
}

func TestShareNodeCloseIsIdempotentAndConcurrent(t *testing.T) {
	mode := sharedMode{Width: 4, Height: 2, Fourcc: "YUYV", BytesPerLine: 8, FPS: 30}
	w := &shareNodeWriter{mode: mode, fd: -1, wakeR: -1, wakeW: -1, done: closedChan()}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = w.Close() }()
		go func() { defer wg.Done(); _ = w.WriteFrame(make([]byte, mode.frameBytes()), 1) }()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := w.WriteFrame(make([]byte, mode.frameBytes()), 1); !errors.Is(err, errShareNodeClosed) {
		t.Fatalf("write after Close: %v, want errShareNodeClosed", err)
	}
}

// countLoopFixture drives pumpShareEvents with a pipe standing in for the node
// descriptor (POLLIN instead of POLLPRI) and a scripted dequeue.
type countLoopFixture struct {
	nodeR, nodeW int
	wakeR, wakeW int
	out          chan uint32
	done         chan struct{}
}

func newCountLoopFixture(t *testing.T, dequeue func() (uint32, uint32, error)) *countLoopFixture {
	t.Helper()
	f := &countLoopFixture{out: make(chan uint32, 1), done: make(chan struct{})}
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	f.nodeR, f.nodeW = p[0], p[1]
	if err := unix.SetNonblock(f.nodeR, true); err != nil {
		t.Fatal(err)
	}
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	f.wakeR, f.wakeW = p[0], p[1]
	t.Cleanup(func() {
		for _, fd := range []int{f.nodeR, f.nodeW, f.wakeR, f.wakeW} {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
		}
	})
	go func() {
		defer close(f.done)
		pumpShareEvents(zap.NewNop(), "fixture", f.nodeR, f.wakeR, unix.POLLIN, func(int) (uint32, uint32, error) {
			// Consume what made the pipe readable so poll does not spin.
			var b [64]byte
			_, _ = unix.Read(f.nodeR, b[:])
			return dequeue()
		}, f.out)
	}()
	return f
}

// waitExit asserts the loop returned and left out closed (a buffered value may
// remain ahead of the close).
func (f *countLoopFixture) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("event loop did not exit")
	}
	for i := 0; i < 2; i++ {
		if _, ok := <-f.out; !ok {
			return
		}
	}
	t.Fatal("Counts() not closed")
}

func TestPumpShareEvents_NewestCountWinsAndWakeExits(t *testing.T) {
	seq := []uint32{1, 2, 3}
	i := 0
	f := newCountLoopFixture(t, func() (uint32, uint32, error) {
		c := seq[i]
		i++
		return c, uint32(len(seq) - i), nil // pending counts down to 0
	})
	if _, err := unix.Write(f.nodeW, []byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-f.out:
		if got != 3 {
			t.Fatalf("got count %d, want the newest (3)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no count delivered")
	}
	if _, err := unix.Write(f.wakeW, []byte{1}); err != nil {
		t.Fatal(err)
	}
	f.waitExit(t)
}

func TestPumpShareEvents_UnreadCountIsOverwritten(t *testing.T) {
	// Dequeue call n starts only after the loop has finished delivering event
	// n-1, so when call 4 begins, counts 1..3 have all been sent and out holds
	// the newest unread one. Call 4 reports nothing queued (EAGAIN) and sends
	// nothing itself.
	var n atomic.Uint32
	reached4 := make(chan struct{})
	f := newCountLoopFixture(t, func() (uint32, uint32, error) {
		c := n.Add(1)
		if c == 4 {
			close(reached4)
			return 0, 0, unix.EAGAIN
		}
		return c, 0, nil
	})
	for k := 0; k < 4; k++ {
		if _, err := unix.Write(f.nodeW, []byte{1}); err != nil {
			t.Fatal(err)
		}
		// Wait for the loop to take this wake so each write is its own event.
		deadline := time.After(2 * time.Second)
		for int(n.Load()) < k+1 {
			select {
			case <-deadline:
				t.Fatal("event loop did not dequeue")
			case <-time.After(time.Millisecond):
			}
		}
	}
	select {
	case <-reached4:
	case <-time.After(2 * time.Second):
		t.Fatal("fourth dequeue never ran")
	}
	if got := <-f.out; got != 3 {
		t.Fatalf("got %d, want 3 (newest overwrites unread)", got)
	}
	_, _ = unix.Write(f.wakeW, []byte{1})
	f.waitExit(t)
}

func TestPumpShareEvents_HangupClosesCounts(t *testing.T) {
	f := newCountLoopFixture(t, func() (uint32, uint32, error) { return 0, 0, nil })
	_ = unix.Close(f.nodeW) // POLLHUP on the read end
	f.nodeW = -1
	f.waitExit(t)
}

func TestPumpShareEvents_ENODEVClosesCounts(t *testing.T) {
	f := newCountLoopFixture(t, func() (uint32, uint32, error) { return 0, 0, unix.ENODEV })
	if _, err := unix.Write(f.nodeW, []byte{1}); err != nil {
		t.Fatal(err)
	}
	f.waitExit(t)
}

func TestShareNodeRequiresFreshStart(t *testing.T) {
	for _, refusal := range []error{nil, unix.EINVAL, unix.ENOTTY} {
		w := &shareNodeWriter{path: "/dev/video100", ioctlFn: func(req uintptr, arg unsafe.Pointer) error {
			control := (*v4l2Control)(arg)
			if req != vidiocSCtrl || control.ID != v4l2LoopbackCidFreshStart || control.Value != 1 {
				t.Fatalf("wrong freshness control: request %x, control %+v", req, control)
			}
			return refusal
		}}
		err := w.requireFreshStart()
		if !errors.Is(err, refusal) {
			t.Fatalf("fresh-start refusal = %v, want %v", err, refusal)
		}
	}
}

func TestShareNodeUnsupportedFreshStartRefusesBeforeFormatOrBuffers(t *testing.T) {
	calls := 0
	w := &shareNodeWriter{path: "/dev/video100", ioctlFn: func(req uintptr, arg unsafe.Pointer) error {
		calls++
		if req != vidiocSCtrl || (*v4l2Control)(arg).ID != v4l2LoopbackCidFreshStart {
			t.Fatal("touched format or buffers before checking freshness support")
		}
		return unix.EINVAL
	}}
	if err := w.configure(); !errors.Is(err, unix.EINVAL) || calls != 1 {
		t.Fatalf("unsupported sharing: calls=%d, error=%v", calls, err)
	}
}

func TestRetainedShareNodeChecksFreshStartWithoutClaimingWriter(t *testing.T) {
	for _, tc := range []struct {
		name            string
		value           int32
		err             error
		wantUnsupported bool
	}{
		{name: "enabled", value: 1},
		{name: "disabled"},
		{name: "unsupported", err: unix.EINVAL, wantUnsupported: true},
		{name: "no ioctl", err: unix.ENOTTY, wantUnsupported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			w := &shareNodeWriter{path: "/dev/video254", ioctlFn: func(req uintptr, arg unsafe.Pointer) error {
				calls++
				control := (*v4l2Control)(arg)
				if req != vidiocGCtrl || control.ID != v4l2LoopbackCidFreshStart {
					t.Fatal("retained-node check modified controls, format or buffers")
				}
				control.Value = tc.value
				return tc.err
			}}
			err := w.checkFreshStart()
			if calls != 1 || (err == nil) != (tc.err == nil && tc.value == 1) || errors.Is(err, errShareFreshStartUnsupported) != tc.wantUnsupported {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}
