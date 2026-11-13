//go:build linux && regression_camera_fixture

package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// This diagnostic runs only after the mandatory IP-camera lifecycle fails.
// It isolates the OUTPUT queue contract without GStreamer, RTSP or a reader.
// In particular, allocating a buffer does not queue it: DQBUF must never return
// a buffer that has not been queued, or return the same queued buffer twice.
// The runner retains the original lifecycle failure regardless of this result.
func TestIPCameraOutputQueueContractDiagnostic(t *testing.T) {
	testIPCameraOutputQueue(t, false)
}

// The candidate module must pass this stricter queue/STREAMOFF/restart gate,
// followed by the unchanged decoded-pixel and shared-camera consumer suites.
// The original pinned module must fail OUTPUT_QUEUE_CONTRACT on this same test.
func TestIPCameraOutputQueueLifecycle(t *testing.T) {
	testIPCameraOutputQueue(t, true)
}

func testIPCameraOutputQueue(t *testing.T, lifecycle bool) {
	mode := "output-queue"
	if lifecycle {
		mode = "output-queue-lifecycle"
	}
	if os.Getenv("WENDY_IPCAM_DELIVERY_DIAGNOSTIC") != mode {
		t.Skip("requires explicit hosted output-queue diagnostic")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("requires disposable hosted Linux media runner")
	}
	const path = "/dev/video200"
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requires absent fixture node %s: %v", path, err)
	}
	version, err := os.ReadFile("/sys/module/v4l2loopback/version")
	if err != nil || strings.TrimSpace(string(version)) != "0.15.4" {
		t.Fatalf("expected job-owned v4l2loopback 0.15.4: %q %v", version, err)
	}
	directory := t.TempDir()
	registry := ipcam.NewRegistry(filepath.Join(directory, "cameras.json"))
	camera, err := registry.Upsert(ipcam.Camera{MAC: "ec:71:db:2a:ae:7e", Address: "127.0.0.1", StreamMain: "/measurement", StreamSub: "/measurement"})
	if err != nil || camera.ID != 200 {
		t.Fatalf("unexpected private registry camera: %+v %v", camera, err)
	}
	// EnsureNodes deliberately excludes unpaired cameras. This private registry
	// must include a stored fixture login before production creates its node.
	credentials := ipcam.NewCredentialStore(filepath.Join(directory, "credentials.json"))
	if err := credentials.Set(camera.MAC, ipcam.Credential{Username: "queue-fixture", Password: "not-a-device-secret"}); err != nil {
		t.Fatalf("store private queue fixture credentials: %v", err)
	}
	loopback := ipcam.NewLoopback(context.Background(), zap.NewNop(), registry, credentials,
		func(context.Context, []string) error {
			t.Error("output-queue diagnostic unexpectedly started a producer")
			return errors.New("no producer permitted")
		})
	drained := true
	t.Cleanup(func() {
		loopback.Shutdown()
		if drained {
			loopback.RemoveCamera(camera.ID)
		} else {
			t.Error("retaining owned video200: blocked ioctl drain unproven")
		}
	})
	if err := loopback.EnsureNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if actual, ok := loopback.NodePath(camera.ID); !ok || actual != path {
		t.Fatalf("unexpected owned node %q %v", actual, ok)
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if drained {
			unix.Close(fd)
		}
	}()
	ioctl := func(request uintptr, argument unsafe.Pointer) unix.Errno {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
		return errno
	}
	format := v4l2Format{Type: v4l2BufTypeVideoOutput, Width: 64, Height: 48,
		PixelFormat: uint32('Y') | uint32('U')<<8 | uint32('Y')<<16 | uint32('V')<<24,
		Field:       v4l2FieldNone, BytesPerLine: 128, SizeImage: 64 * 48 * 2}
	if errno := ioctl(vidiocSFmt, unsafe.Pointer(&format)); errno != 0 {
		t.Fatalf("S_FMT: %v", errno)
	}
	if format.Width != 64 || format.Height != 48 || format.SizeImage == 0 {
		t.Fatalf("unexpected negotiated format: %+v", format)
	}
	request := v4l2ReqBuffers{Count: 3, Type: v4l2BufTypeVideoOutput, Memory: v4l2MemoryMmap}
	if errno := ioctl(vidiocReqbufs, unsafe.Pointer(&request)); errno != 0 {
		t.Fatalf("REQBUFS: %v", errno)
	}
	if request.Count < 2 || request.Count > 3 {
		t.Fatalf("expected two or three allocated buffers, got %d", request.Count)
	}
	for index := uint32(0); index < request.Count; index++ {
		var buffer v4l2Buf
		buffer.setIndex(index)
		buffer.setType(v4l2BufTypeVideoOutput)
		buffer.setMemory(v4l2MemoryMmap)
		if errno := ioctl(vidiocQuerybuf, unsafe.Pointer(&buffer)); errno != 0 {
			t.Fatalf("QUERYBUF %d: %v", index, errno)
		}
		memory, err := unix.Mmap(fd, int64(buffer.offset()), int(v4l2BufLength(&buffer)), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if !drained {
				return
			}
			if err := unix.Munmap(memory); err != nil {
				t.Errorf("unmap owned output buffer: %v", err)
			}
		}()
		clear(memory)
	}
	var queued v4l2Buf
	queued.setType(v4l2BufTypeVideoOutput)
	queued.setMemory(v4l2MemoryMmap)
	queued.setIndex(0)
	v4l2BufSetBytesUsed(&queued, format.SizeImage)
	v4l2BufSetField(&queued, v4l2FieldNone)
	if errno := ioctl(vidiocQbuf, unsafe.Pointer(&queued)); errno != 0 {
		t.Fatalf("QBUF index=0: %v", errno)
	}
	kind := int32(v4l2BufTypeVideoOutput)
	if errno := ioctl(vidiocStreamon, unsafe.Pointer(&kind)); errno != 0 {
		t.Fatalf("STREAMON: %v", errno)
	}
	defer func() {
		if errno := ioctl(vidiocStreamoff, unsafe.Pointer(&kind)); errno != 0 {
			t.Errorf("STREAMOFF owned output: %v", errno)
		}
	}()
	t.Logf("OUTPUT_QUEUE_SETUP allocated=%d queued=[0] format=%dx%d size=%d nonblocking=true", request.Count, format.Width, format.Height, format.SizeImage)
	for attempt := 0; attempt < 2; attempt++ {
		var dequeued v4l2Buf
		dequeued.setType(v4l2BufTypeVideoOutput)
		dequeued.setMemory(v4l2MemoryMmap)
		errno := ioctl(vidiocDqbuf, unsafe.Pointer(&dequeued))
		t.Logf("OUTPUT_QUEUE_DQBUF attempt=%d index=%d flags=%#x errno=%v", attempt, dequeued.index(), dequeued.flags(), errno)
		if errno == unix.EAGAIN {
			if !lifecycle {
				return
			} // Legal/inconclusive for the diagnostic.
			if attempt != 1 {
				t.Fatal("completed QBUF(0) was not returned")
			}
			break
		}
		if errno != 0 {
			t.Fatalf("DQBUF: %v", errno)
		}
		if dequeued.index() != 0 || attempt != 0 {
			t.Fatalf("OUTPUT_QUEUE_CONTRACT: DQBUF returned index=%d after only QBUF(0), prior_dequeues=%d", dequeued.index(), attempt)
		}
	}
	if !lifecycle {
		return
	}
	queue := func(index uint32) {
		queued.setIndex(index)
		if errno := ioctl(vidiocQbuf, unsafe.Pointer(&queued)); errno != 0 {
			t.Fatalf("QBUF(%d): %v", index, errno)
		}
	}
	dequeue := func(want int) {
		var buffer v4l2Buf
		buffer.setType(v4l2BufTypeVideoOutput)
		buffer.setMemory(v4l2MemoryMmap)
		errno := ioctl(vidiocDqbuf, unsafe.Pointer(&buffer))
		if want < 0 {
			if errno != unix.EAGAIN {
				t.Fatalf("empty output queue: index=%d errno=%v", buffer.index(), errno)
			}
		} else if errno != 0 || buffer.index() != uint32(want) {
			t.Fatalf("output completion want=%d got=%d errno=%v", want, buffer.index(), errno)
		}
	}
	assertPoll := func(writable bool) {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		_, err := unix.Poll(poll, 0)
		if err != nil || poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 || (poll[0].Revents&unix.POLLOUT != 0) != writable {
			t.Fatalf("MMAP output readiness want_writable=%v: %+v %v", writable, poll, err)
		}
	}
	queue(1)
	assertPoll(true)
	dequeue(1)
	dequeue(-1)
	assertPoll(false)
	// Opening a real CAPTURE allocator must not erase an OUTPUT completion.
	// Both sides share the driver's allocated ring, but not done-queue ownership.
	queue(0)
	captureFD, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	captureRequest := v4l2ReqBuffers{Count: 2, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	_, _, captureErr := unix.Syscall(unix.SYS_IOCTL, uintptr(captureFD), vidiocReqbufs, uintptr(unsafe.Pointer(&captureRequest)))
	unix.Close(captureFD)
	if captureErr != 0 {
		t.Fatalf("CAPTURE REQBUFS beside producer: %v", captureErr)
	}
	dequeue(0)
	dequeue(-1)
	// Preserve Wendy's no-DQBUF producer: repeatedly reusing a mapped output
	// buffer must retain one latest completion, not duplicate list entries.
	for index := 0; index < 6; index++ {
		queue(uint32(index % 2))
	}
	dequeue(0)
	dequeue(1)
	dequeue(-1)
	queue(0)
	if errno := ioctl(vidiocStreamoff, unsafe.Pointer(&kind)); errno != 0 {
		t.Fatal(errno)
	}
	if errno := ioctl(vidiocStreamon, unsafe.Pointer(&kind)); errno != 0 {
		t.Fatal(errno)
	}
	dequeue(-1) // STREAMOFF must cancel the pending completion.
	assertPoll(false)
	if err := unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	tids := make(chan int, 1)
	done := make(chan unix.Errno, 1)
	drained = false
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		var buffer v4l2Buf
		buffer.setType(v4l2BufTypeVideoOutput)
		buffer.setMemory(v4l2MemoryMmap)
		tids <- unix.Gettid()
		done <- ioctl(vidiocDqbuf, unsafe.Pointer(&buffer))
	}()
	// Registered after STREAMOFF's general cleanup, so the waiter is joined
	// before maps/fd/node can be released on every failure path.
	defer func() {
		if drained {
			return
		}
		ioctl(vidiocStreamoff, unsafe.Pointer(&kind))
		select {
		case <-done:
			drained = true
		case <-time.After(time.Second):
			t.Error("blocking OUTPUT waiter failed to drain")
		}
	}()
	tid := <-tids
	deadline := time.Now().Add(time.Second)
	for {
		select {
		case errno := <-done:
			drained = true
			t.Fatalf("empty blocking DQBUF returned before STREAMOFF: %v", errno)
		default:
		}
		state, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/syscall", tid))
		fields := strings.Fields(string(state))
		if err == nil && len(fields) >= 3 {
			call, _ := strconv.ParseUint(fields[0], 0, 64)
			callFD, _ := strconv.ParseUint(fields[1], 0, 64)
			request, _ := strconv.ParseUint(fields[2], 0, 64)
			if call == uint64(unix.SYS_IOCTL) && callFD == uint64(fd) && request == vidiocDqbuf {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not observe owned thread blocked in exact DQBUF: %q %v", state, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if errno := ioctl(vidiocStreamoff, unsafe.Pointer(&kind)); errno != 0 {
		t.Fatal(errno)
	}
	select {
	case errno := <-done:
		drained = true
		if errno != unix.EAGAIN {
			t.Fatalf("STREAMOFF waiter result: %v", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("STREAMOFF did not wake exact blocked DQBUF")
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	if errno := ioctl(vidiocStreamon, unsafe.Pointer(&kind)); errno != 0 {
		t.Fatal(errno)
	}
	queue(1)
	dequeue(1)
	dequeue(-1)
	t.Log("OUTPUT_QUEUE_LIFECYCLE_VERIFIED: exact completions, no duplicates, MMAP readiness, STREAMOFF wake, restart")

}
