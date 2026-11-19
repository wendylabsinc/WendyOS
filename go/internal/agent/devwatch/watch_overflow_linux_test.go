//go:build linux

package devwatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// inotifyRecord is one event as the kernel lays it out, with its name padded
// with NULs to len bytes.
func inotifyRecord(mask uint32, name string, padded int) []byte {
	ev := unix.InotifyEvent{Wd: 1, Mask: mask, Len: uint32(padded)}
	if mask&unix.IN_Q_OVERFLOW != 0 {
		ev.Wd = -1
	}
	head := unsafe.Slice((*byte)(unsafe.Pointer(&ev)), unix.SizeofInotifyEvent)
	out := append([]byte(nil), head...)
	nameBytes := make([]byte, padded)
	copy(nameBytes, name)
	return append(out, nameBytes...)
}

// An overflow record carries no name; it is reported before the name filter,
// and the events around it still are.
func TestDispatch_ReportsAnOverflow(t *testing.T) {
	var buf []byte
	buf = append(buf, inotifyRecord(unix.IN_CREATE, "video3", 16)...)
	buf = append(buf, inotifyRecord(unix.IN_Q_OVERFLOW, "", 0)...)
	buf = append(buf, inotifyRecord(unix.IN_CREATE, "media0", 16)...)
	buf = append(buf, inotifyRecord(unix.IN_DELETE, "video4", 16)...)
	var got []Event
	dispatch(buf, func(e Event) { got = append(got, e) })
	want := []Event{{Name: "video3", Created: true}, {Overflow: true}, {Name: "video4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
}

// overflowTestEnv opts in to TestWatch_ReportsARealOverflow.
const overflowTestEnv = "WENDY_DEVWATCH_OVERFLOW_TEST"

// A real overflow: with the kernel's queue limit lowered, a reader held up
// while nodes keep appearing gets IN_Q_OVERFLOW, which Watch reports.
//
// fs.inotify.max_queued_events is one value for the whole kernel (no user or
// mount namespace isolates it), so lowering it affects every inotify instance
// made meanwhile on the machine. The test therefore runs only when
// WENDY_DEVWATCH_OVERFLOW_TEST=1 is set, meant for a disposable privileged
// container or VM, and needs root and a writable /proc/sys there. It puts the
// value back as soon as its own watch is set up, and in t.Cleanup.
func TestWatch_ReportsARealOverflow(t *testing.T) {
	if os.Getenv(overflowTestEnv) != "1" {
		t.Skipf("lowers a kernel-wide inotify limit; set %s=1 to run it (a disposable privileged container)", overflowTestEnv)
	}
	const knob = "/proc/sys/fs/inotify/max_queued_events"
	old, err := os.ReadFile(knob)
	if err != nil {
		t.Fatalf("%s=1, but reading %s failed: %v", overflowTestEnv, knob, err)
	}
	if err := os.WriteFile(knob, []byte("8"), 0); err != nil {
		t.Fatalf("%s=1, but lowering %s failed (it needs root and a writable /proc/sys): %v", overflowTestEnv, knob, err)
	}
	// The limit is read when the inotify instance is made, so it is put back
	// as soon as Watch is set up rather than at the end.
	restore := sync.OnceFunc(func() {
		if err := os.WriteFile(knob, []byte(strings.TrimSpace(string(old))), 0); err != nil {
			t.Errorf("restoring %s: %v", knob, err)
		}
	})
	t.Cleanup(restore)

	dir := t.TempDir()
	events := make(chan Event, 1024)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Watch(ctx, dir, func(e Event) {
			// The first event holds the reader up until the queue overflows.
			once.Do(func() { close(started); <-release })
			events <- e
		})
	}()
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	defer letGo()

	for i := 0; ; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("video%d", 900+i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(20 * time.Millisecond):
			if i > 200 {
				t.Fatal("the watch never reported a node")
			}
			continue
		}
		break
	}
	restore()
	for i := 0; i < 64; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("video%d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	letGo()
	deadline := time.After(2 * time.Second)
	for overflowed := false; !overflowed; {
		select {
		case e := <-events:
			overflowed = e.Overflow
		case <-deadline:
			t.Fatal("no overflow reported after 64 nodes appeared behind a queue of 8")
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Watch returned %v after cancel, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after cancel")
	}
}
