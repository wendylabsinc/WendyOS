package services

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBlankFrame_IsBlackInEveryFormat(t *testing.T) {
	cases := []struct {
		fourcc string
		bpl    uint32
		first4 []byte
	}{
		{"YUYV", 1280, []byte{0x10, 0x80, 0x10, 0x80}},
		{"UYVY", 1280, []byte{0x80, 0x10, 0x80, 0x10}},
		{"GREY", 640, []byte{0, 0, 0, 0}},
		{"Y16 ", 1280, []byte{0, 0, 0, 0}},
	}
	for _, c := range cases {
		mode := sharedMode{Width: 640, Height: 480, Fourcc: c.fourcc, BytesPerLine: c.bpl, FPS: 30}
		got := blankFrame(mode)
		if len(got) != mode.frameBytes() {
			t.Errorf("%s: length %d, want %d", c.fourcc, len(got), mode.frameBytes())
			continue
		}
		if !bytes.Equal(got[:4], c.first4) {
			t.Errorf("%s: first bytes % x, want % x", c.fourcc, got[:4], c.first4)
		}
		if !bytes.Equal(got[len(got)-4:], c.first4) {
			t.Errorf("%s: last bytes % x, want % x", c.fourcc, got[len(got)-4:], c.first4)
		}
	}
}

func TestShareNodeIoctlNumbers(t *testing.T) {
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
		go func() { defer wg.Done(); _ = w.WriteBlank(1) }()
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
		pumpShareEvents(f.nodeR, f.wakeR, unix.POLLIN, func(int) (uint32, uint32, error) {
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
	n := uint32(0)
	delivered := make(chan struct{}, 4)
	f := newCountLoopFixture(t, func() (uint32, uint32, error) {
		n++
		delivered <- struct{}{}
		return n, 0, nil
	})
	for k := 0; k < 3; k++ {
		if _, err := unix.Write(f.nodeW, []byte{1}); err != nil {
			t.Fatal(err)
		}
		<-delivered
	}
	time.Sleep(20 * time.Millisecond)
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
