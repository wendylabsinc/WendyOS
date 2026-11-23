//go:build linux && regression_camera_fixture

package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const compatibilityFrameSize = 64 * 48 * 2
const compatibilityControlBase = 0x0098f900 // V4L2_CID_USER_BASE | 0xf000, pinned upstream.

func compatibilityIoctl(fd int, request uintptr, value unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func compatibilityControl(fd int, offset uint32, value int32) error {
	control := struct {
		ID    uint32
		Value int32
	}{compatibilityControlBase + offset, value}
	if err := compatibilityIoctl(fd, 0xc008561c, unsafe.Pointer(&control)); err != nil {
		return fmt.Errorf("S_CTRL %d: %w", offset, err)
	}
	if offset != 3 { // timeout_image_io is a button rather than a readable value.
		control.Value = -1
		if err := compatibilityIoctl(fd, 0xc008561b, unsafe.Pointer(&control)); err != nil || control.Value != value {
			return fmt.Errorf("G_CTRL %d=%d want=%d: %v", offset, control.Value, value, err)
		}
	}
	return nil
}

type compatibilityWriter struct {
	fd   int
	maps [][]byte
	next uint32
}

func newCompatibilityWriter(path string) (_ *compatibilityWriter, result error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	writer := &compatibilityWriter{fd: fd}
	defer func() {
		if result != nil {
			_ = writer.close()
		}
	}()
	format := v4l2Format{Type: v4l2BufTypeVideoOutput, Width: 64, Height: 48, PixelFormat: uint32('Y') | uint32('U')<<8 | uint32('Y')<<16 | uint32('V')<<24, Field: v4l2FieldNone, BytesPerLine: 128, SizeImage: compatibilityFrameSize}
	if err = compatibilityIoctl(fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
		return nil, err
	}
	if format.Width != 64 || format.Height != 48 || format.SizeImage != compatibilityFrameSize {
		return nil, fmt.Errorf("unexpected negotiated format %+v", format)
	}
	request := v4l2ReqBuffers{Count: 2, Type: v4l2BufTypeVideoOutput, Memory: v4l2MemoryMmap}
	if err = compatibilityIoctl(fd, vidiocReqbufs, unsafe.Pointer(&request)); err != nil {
		return nil, err
	}
	if request.Count != 2 {
		return nil, fmt.Errorf("expected actual two-buffer ring, got %d", request.Count)
	}
	for index := uint32(0); index < request.Count; index++ {
		var buffer v4l2Buf
		buffer.setIndex(index)
		buffer.setType(v4l2BufTypeVideoOutput)
		buffer.setMemory(v4l2MemoryMmap)
		if err = compatibilityIoctl(fd, vidiocQuerybuf, unsafe.Pointer(&buffer)); err != nil {
			return nil, err
		}
		memory, e := unix.Mmap(fd, int64(buffer.offset()), int(v4l2BufLength(&buffer)), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if e != nil {
			return nil, e
		}
		writer.maps = append(writer.maps, memory)
	}
	kind := int32(v4l2BufTypeVideoOutput)
	if err = compatibilityIoctl(fd, vidiocStreamon, unsafe.Pointer(&kind)); err != nil {
		return nil, err
	}
	return writer, nil
}
func (w *compatibilityWriter) close() error {
	if w.fd < 0 {
		return nil
	}
	kind := int32(v4l2BufTypeVideoOutput)
	result := compatibilityIoctl(w.fd, vidiocStreamoff, unsafe.Pointer(&kind))
	for _, memory := range w.maps {
		result = errors.Join(result, unix.Munmap(memory))
	}
	w.maps = nil
	result = errors.Join(result, unix.Close(w.fd))
	w.fd = -1
	return result
}
func (w *compatibilityWriter) queue(value byte) error {
	index := w.next % uint32(len(w.maps))
	w.next++
	for i := range w.maps[index] {
		w.maps[index][i] = value
	}
	var buffer v4l2Buf
	buffer.setIndex(index)
	buffer.setType(v4l2BufTypeVideoOutput)
	buffer.setMemory(v4l2MemoryMmap)
	v4l2BufSetBytesUsed(&buffer, compatibilityFrameSize)
	v4l2BufSetField(&buffer, v4l2FieldNone)
	if err := compatibilityIoctl(w.fd, vidiocQbuf, unsafe.Pointer(&buffer)); err != nil {
		return err
	}
	buffer = v4l2Buf{}
	buffer.setType(v4l2BufTypeVideoOutput)
	buffer.setMemory(v4l2MemoryMmap)
	if err := compatibilityIoctl(w.fd, vidiocDqbuf, unsafe.Pointer(&buffer)); err != nil || buffer.index() != index {
		return fmt.Errorf("actual output completion got=%d want=%d: %v", buffer.index(), index, err)
	}
	return w.empty()
}
func (w *compatibilityWriter) empty() error {
	var buffer v4l2Buf
	buffer.setType(v4l2BufTypeVideoOutput)
	buffer.setMemory(v4l2MemoryMmap)
	err := compatibilityIoctl(w.fd, vidiocDqbuf, unsafe.Pointer(&buffer))
	if !errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("timer/reader fabricated output completion index=%d: %v", buffer.index(), err)
	}
	return nil
}
func compatibilityRead(fd int) ([]byte, error) {
	frame := make([]byte, compatibilityFrameSize)
	n, err := unix.Read(fd, frame)
	if err != nil {
		return nil, err
	}
	if n != len(frame) {
		return nil, fmt.Errorf("short captured frame %d", n)
	}
	return frame, nil
}
func compatibilityWaitFrame(fd int, value byte, timeout time.Duration) error {
	want := bytes.Repeat([]byte{value}, compatibilityFrameSize)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := compatibilityRead(fd)
		if err == nil && bytes.Equal(frame, want) {
			return nil
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("actual captured phase %#x did not arrive", value)
}
func compatibilityTimeoutImage(path string, w *compatibilityWriter, value byte) error {
	if err := compatibilityControl(w.fd, 3, 0); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	request := v4l2ReqBuffers{Count: 1, Type: v4l2BufTypeVideoOutput, Memory: v4l2MemoryMmap}
	if err = compatibilityIoctl(fd, vidiocReqbufs, unsafe.Pointer(&request)); err != nil {
		return err
	}
	var buffer v4l2Buf
	buffer.setType(v4l2BufTypeVideoOutput)
	buffer.setMemory(v4l2MemoryMmap)
	if err = compatibilityIoctl(fd, vidiocQuerybuf, unsafe.Pointer(&buffer)); err != nil {
		return err
	}
	memory, err := unix.Mmap(fd, int64(buffer.offset()), int(v4l2BufLength(&buffer)), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	defer unix.Munmap(memory)
	for i := range memory {
		memory[i] = value
	}
	v4l2BufSetBytesUsed(&buffer, compatibilityFrameSize)
	return compatibilityIoctl(fd, vidiocQbuf, unsafe.Pointer(&buffer))
}

type compatibilityWriterCommand struct {
	restart bool
	value   byte
	ack     chan error
	gate    <-chan struct{}
}
type compatibilityReaderCommand struct {
	generation int
	ack        chan error
	gate       <-chan struct{}
}
type compatibilityObservation struct {
	generation int
	value      byte
}

func compatibilityRestartJourney(t *testing.T, path string, drained *bool) {
	t.Helper()
	initialWriter, err := newCompatibilityWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	initialReader, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = initialWriter.close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writerCommands := make(chan compatibilityWriterCommand)
	readerCommands := make(chan compatibilityReaderCommand)
	observations := make(chan compatibilityObservation, 64)
	failures := make(chan error, 2)
	writerDone, readerDone := make(chan struct{}), make(chan struct{})
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	*drained = false
	// Only each worker touches its own descriptor/mappings. Do not remove a node
	// beneath an uncertain ioctl: the outer process guardian remains the bound.
	defer func() {
		cancel()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for _, done := range []<-chan struct{}{writerDone, readerDone} {
			select {
			case <-done:
			case <-timer.C:
				t.Error("OUTPUT_COMPATIBILITY_DRAIN_UNKNOWN: worker did not join")
				return
			}
		}
		*drained = true
		select {
		case err := <-failures:
			t.Error(err)
		default:
		}
	}()
	go func() {
		defer close(writerDone)
		writer := initialWriter
		var err error
		defer func() {
			if err := writer.close(); err != nil {
				fail(err)
			}
		}()
		value := byte(0x51)
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case command := <-writerCommands:
				if command.gate != nil {
					select {
					case <-command.gate:
					case <-ctx.Done():
						return
					}
				}
				if command.restart {
					err = writer.close()
					if err == nil {
						var replacement *compatibilityWriter
						replacement, err = newCompatibilityWriter(path)
						if err == nil {
							writer = replacement
						}
					}
				}
				if err == nil {
					value = command.value
					err = writer.queue(value)
				}
				command.ack <- err
				if err != nil {
					fail(err)
					return
				}
			case <-ticker.C:
				if err = writer.queue(value); err != nil {
					fail(err)
					return
				}
			}
		}
	}()
	go func() {
		defer close(readerDone)
		fd := initialReader
		var err error
		defer func() {
			if fd >= 0 {
				if err := unix.Close(fd); err != nil {
					fail(err)
				}
			}
		}()
		generation := 1
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case command := <-readerCommands:
				if command.gate != nil {
					select {
					case <-command.gate:
					case <-ctx.Done():
						return
					}
				}
				err = unix.Close(fd)
				fd = -1
				if err == nil {
					fd, err = unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				}
				generation = command.generation
				command.ack <- err
				if err != nil {
					fail(err)
					return
				}
			case <-ticker.C:
				frame, readErr := compatibilityRead(fd)
				if errors.Is(readErr, unix.EAGAIN) {
					continue
				}
				if readErr != nil {
					fail(readErr)
					return
				}
				// A phase may change while an older frame is being delivered. Only a
				// complete exact new frame can satisfy the next generation's observation.
				value := frame[0]
				if value < 0x51 || value > 0x55 || !bytes.Equal(frame, bytes.Repeat([]byte{value}, compatibilityFrameSize)) {
					continue
				}
				select {
				case observations <- compatibilityObservation{generation, value}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	waitAck := func(ack <-chan error) {
		t.Helper()
		select {
		case err := <-ack:
			if err != nil {
				t.Fatal(err)
			}
		case err := <-failures:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("actual stream restart did not acknowledge")
		}
	}
	sendWriter := func(restart bool, value byte, gate <-chan struct{}) <-chan error {
		t.Helper()
		ack := make(chan error, 1)
		select {
		case writerCommands <- compatibilityWriterCommand{restart, value, ack, gate}:
		case err := <-failures:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("writer did not accept restart")
		}
		return ack
	}
	sendReader := func(generation int, gate <-chan struct{}) <-chan error {
		t.Helper()
		ack := make(chan error, 1)
		select {
		case readerCommands <- compatibilityReaderCommand{generation, ack, gate}:
		case err := <-failures:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("reader did not accept restart")
		}
		return ack
	}
	observe := func(generation int, value byte) {
		t.Helper()
		count := 0
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for count < 3 {
			select {
			case got := <-observations:
				if got.generation == generation && got.value == value {
					count++
				}
			case err := <-failures:
				t.Fatal(err)
			case <-timer.C:
				t.Fatalf("actual concurrent stream did not deliver phase=%#x reader_generation=%d count=%d", value, generation, count)
			}
		}
		t.Logf("OUTPUT_RESTART_PHASE_VERIFIED reader_generation=%d phase=%#x actual_frames=%d", generation, value, count)
	}
	observe(1, 0x51)
	waitAck(sendWriter(true, 0x52, nil))
	observe(1, 0x52)
	waitAck(sendReader(2, nil))
	waitAck(sendWriter(false, 0x53, nil))
	observe(2, 0x53)
	// Both workers receive a restart before the shared barrier opens. They
	// independently close/reopen their own descriptors while the peer is live.
	gate := make(chan struct{})
	writerAck := sendWriter(true, 0x54, gate)
	readerAck := sendReader(3, gate)
	close(gate)
	waitAck(writerAck)
	waitAck(readerAck)
	observe(3, 0x54)
	waitAck(sendWriter(false, 0x55, nil))
	observe(3, 0x55)
}

func TestV4L2OutputQueueCompatibility(t *testing.T) {
	if os.Getenv("WENDY_OUTPUT_QUEUE_COMPATIBILITY") != "1" {
		t.Skip("requires the candidate module hosted compatibility gate")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Getenv("WENDY_LOOPBACK_JOB_OWNED") != "1" || os.Geteuid() != 0 {
		t.Fatal("requires disposable hosted job-owned kernel module")
	}
	const path = "/dev/video200"
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture node already exists", err)
	}
	version, err := os.ReadFile("/sys/module/v4l2loopback/version")
	if err != nil || strings.TrimSpace(string(version)) != "0.15.4" {
		t.Fatal("wrong job-owned module", err)
	}
	directory := t.TempDir()
	registry := ipcam.NewRegistry(filepath.Join(directory, "cameras.json"))
	credentials := ipcam.NewCredentialStore(filepath.Join(directory, "credentials.json"))
	camera, err := registry.Upsert(ipcam.Camera{MAC: "ec:71:db:2a:ae:7e", Address: "127.0.0.1", StreamMain: "/compatibility", StreamSub: "/compatibility"})
	if err != nil || camera.ID != 200 {
		t.Fatal("private camera allocation", err)
	}
	if err = credentials.Set(camera.MAC, ipcam.Credential{Username: "owned-queue-fixture", Password: "not-a-device-secret"}); err != nil {
		t.Fatal(err)
	}
	loopback := ipcam.NewLoopback(context.Background(), zap.NewNop(), registry, credentials, func(context.Context, []string) error {
		return errors.New("compatibility test never starts an external producer")
	})
	drained := true
	t.Cleanup(func() {
		loopback.Shutdown()
		if !drained {
			t.Error("OUTPUT_COMPATIBILITY_DRAIN_UNKNOWN: retaining owned node")
			return
		}
		loopback.RemoveCamera(camera.ID)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Error("owned compatibility node remains", err)
		}
	})
	if err = loopback.EnsureNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if actual, ok := loopback.NodePath(camera.ID); !ok || actual != path {
		t.Fatal("unexpected production-created node", actual)
	}
	t.Run("sustain-and-timeout", func(t *testing.T) {
		writer, err := newCompatibilityWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := writer.close(); err != nil {
				t.Error(err)
			}
		}()
		reader, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(reader)
		parm := v4l2StreamParm{Type: v4l2BufTypeVideoOutput, TpfNumerator: 1, TpfDenominator: 10}
		if err = compatibilityIoctl(writer.fd, vidiocSParm, unsafe.Pointer(&parm)); err != nil || parm.TpfNumerator != 1 || parm.TpfDenominator != 10 {
			t.Fatalf("actual10fps negotiation %+v: %v", parm, err)
		}
		if err = compatibilityControl(writer.fd, 1, 1); err != nil {
			t.Fatal(err)
		}
		if err = writer.queue(0x41); err != nil {
			t.Fatal(err)
		}
		if err = compatibilityWaitFrame(reader, 0x41, time.Second); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		count := 0
		var last time.Duration
		for time.Since(started) < 700*time.Millisecond {
			frame, err := compatibilityRead(reader)
			if err == nil {
				if !bytes.Equal(frame, bytes.Repeat([]byte{0x41}, compatibilityFrameSize)) {
					t.Fatal("sustained capture changed last real frame")
				}
				count++
				last = time.Since(started)
			} else if !errors.Is(err, unix.EAGAIN) {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if count < 3 || last < 200*time.Millisecond {
			t.Fatalf("sustain_framerate did not deliver timed repeated frames count=%d last=%s", count, last)
		}
		if err = writer.empty(); err != nil {
			t.Fatal(err)
		}
		t.Logf("OUTPUT_SUSTAIN_VERIFIED requested_fps=10 repeated=%d observation=%s last=%s output_completions=0", count, time.Since(started), last)
		if err = compatibilityControl(writer.fd, 1, 0); err != nil {
			t.Fatal(err)
		}
		if err = compatibilityControl(writer.fd, 2, 400); err != nil {
			t.Fatal(err)
		}
		if err = compatibilityTimeoutImage(path, writer, 0xd3); err != nil {
			t.Fatal(err)
		}
		if err = writer.queue(0x42); err != nil {
			t.Fatal(err)
		}
		written := time.Now()
		if err = compatibilityWaitFrame(reader, 0x42, time.Second); err != nil {
			t.Fatal(err)
		}
		if err = compatibilityWaitFrame(reader, 0xd3, 2*time.Second); err != nil {
			t.Fatal(err)
		}
		if time.Since(written) < 200*time.Millisecond {
			t.Fatal("timeout image appeared before the qualified idle interval")
		}
		if err = writer.empty(); err != nil {
			t.Fatal(err)
		}
		if err = writer.queue(0x43); err != nil {
			t.Fatal(err)
		}
		if err = compatibilityWaitFrame(reader, 0x43, time.Second); err != nil {
			t.Fatal(err)
		}
		if err = compatibilityControl(writer.fd, 2, 0); err != nil {
			t.Fatal(err)
		}
		t.Log("OUTPUT_TIMEOUT_IMAGE_VERIFIED configured_image=0xd3 idle_ms=400 fresh_writer_recovered=true output_completions=0")
	})
	if t.Failed() {
		return
	}
	t.Run("concurrent-restarts", func(t *testing.T) { compatibilityRestartJourney(t, path, &drained) })
	if !t.Failed() {
		t.Log("OUTPUT_QUEUE_COMPATIBILITY_VERIFIED actual kernel timers, timeout image and concurrent stream restart")
	}
}
