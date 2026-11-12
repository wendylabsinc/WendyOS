package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const testLayoutsLine = "zephyr-1 loader=0x08000000-0x080d0000 sketch=0x08100000-0x081c0000\n"

var testZephyr1 = mcuLayout{Name: "zephyr-1", LoaderStart: 0x08000000, LoaderEnd: 0x080d0000,
	SketchStart: 0x08100000, SketchEnd: 0x081c0000}

type elfSegment struct{ paddr, filesz, memsz uint32 }

// testELF builds a minimal 32-bit ARM ELF with one PT_LOAD per segment.
func testELF(segs ...elfSegment) []byte {
	const ehsize, phentsize = 52, 32
	var b bytes.Buffer
	ident := [16]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS32), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)}
	b.Write(ident[:])
	le := binary.LittleEndian
	w := func(v any) { _ = binary.Write(&b, le, v) }
	w(uint16(elf.ET_EXEC))
	w(uint16(elf.EM_ARM))
	w(uint32(elf.EV_CURRENT))
	w(uint32(0x08000000)) // entry
	w(uint32(ehsize))     // phoff
	w(uint32(0))          // shoff
	w(uint32(0))          // flags
	w(uint16(ehsize))
	w(uint16(phentsize))
	w(uint16(len(segs)))
	w(uint16(0)) // shentsize
	w(uint16(0)) // shnum
	w(uint16(0)) // shstrndx
	off := uint32(ehsize + phentsize*len(segs))
	for _, s := range segs {
		w(uint32(elf.PT_LOAD))
		w(off)
		w(s.paddr) // vaddr
		w(s.paddr)
		w(s.filesz)
		w(s.memsz)
		w(uint32(elf.PF_R | elf.PF_X))
		w(uint32(4))
		off += s.filesz
	}
	for _, s := range segs {
		b.Write(make([]byte, s.filesz))
	}
	return b.Bytes()
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type helperCall struct {
	args []string
	ctx  context.Context
}

type fakeHelper struct {
	mu      sync.Mutex
	calls   []helperCall
	output  string
	waitErr error
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *fakeHelper) run(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	f.mu.Lock()
	f.calls = append(f.calls, helperCall{args: args, ctx: ctx})
	f.mu.Unlock()
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		<-f.release
	}
	return io.NopCloser(strings.NewReader(f.output)), func() error { return f.waitErr }, nil
}

// callCount counts flashes, not the restarts after a timed-out one.
func (f *fakeHelper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if !strings.HasPrefix(c.args[0], "--") {
			n++
		}
	}
	return n
}

const okStatus = `wendy-mcu-status {"ok":true,"loader_written":false,"sketch_written":true,"router_active":true,"error":""}`

func newTestMcuService(t *testing.T, h *fakeHelper) *McuService {
	t.Helper()
	s := NewMcuService(zap.NewNop())
	s.stagingDir = filepath.Join(t.TempDir(), "staging")
	s.statePath = filepath.Join(t.TempDir(), "mcu", "state.json")
	s.layouts = func() []mcuLayout { return []mcuLayout{testZephyr1} }
	s.runHelper = h.run
	s.holdUpdater = func(*zap.Logger) func() { return func() {} }
	return s
}

func flashRequests(spec *agentpbv2.McuFirmwareSpec, data []byte, chunk int) []*agentpbv2.FlashFirmwareRequest {
	reqs := []*agentpbv2.FlashFirmwareRequest{{RequestType: &agentpbv2.FlashFirmwareRequest_Spec{Spec: spec}}}
	for len(data) > 0 {
		n := min(chunk, len(data))
		reqs = append(reqs, &agentpbv2.FlashFirmwareRequest{RequestType: &agentpbv2.FlashFirmwareRequest_Chunk_{
			Chunk: &agentpbv2.FlashFirmwareRequest_Chunk{Data: data[:n]}}})
		data = data[n:]
	}
	return reqs
}

type flashInput struct {
	loader, sketch []byte
	spec           *agentpbv2.McuFirmwareSpec
}

func validFlash() flashInput {
	loader := testELF(elfSegment{0x08000000, 64, 64}, elfSegment{0x20000000, 0, 128})
	sketch := bytes.Repeat([]byte{0xa5}, 300)
	return flashInput{loader: loader, sketch: sketch, spec: &agentpbv2.McuFirmwareSpec{
		Layout: "zephyr-1", AppId: "new.app",
		Loader: &agentpbv2.McuImage{Size: uint64(len(loader)), Sha256: sha(loader)},
		Sketch: &agentpbv2.McuImage{Size: uint64(len(sketch)), Sha256: sha(sketch)},
	}}
}

type flashStream = fakeBidiStream[agentpbv2.FlashFirmwareRequest, agentpbv2.FlashFirmwareResponse]

func runFlash(ctx context.Context, s *McuService, reqs []*agentpbv2.FlashFirmwareRequest) (*flashStream, error) {
	ch := make(chan *agentpbv2.FlashFirmwareRequest, len(reqs))
	for _, r := range reqs {
		ch <- r
	}
	close(ch)
	stream := &flashStream{ctx: ctx, recv: ch}
	return stream, s.FlashFirmware(stream)
}

func lastResponse(t *testing.T, stream *flashStream) *agentpbv2.FlashFirmwareResponse {
	t.Helper()
	if len(stream.sent) == 0 {
		t.Fatal("no responses sent")
	}
	return stream.sent[len(stream.sent)-1]
}

func stagingEmpty(t *testing.T, s *McuService) {
	t.Helper()
	entries, _ := os.ReadDir(s.stagingDir)
	if len(entries) != 0 {
		t.Errorf("staging dir not cleaned: %v", entries)
	}
}

func TestParseMCULayouts(t *testing.T) {
	got, err := parseMCULayouts(testLayoutsLine)
	if err != nil {
		t.Fatalf("parseMCULayouts: %v", err)
	}
	if len(got) != 1 || got[0] != testZephyr1 {
		t.Fatalf("parseMCULayouts = %+v, want [%+v]", got, testZephyr1)
	}
	if _, err := parseMCULayouts("zephyr-1 loader=nope\n"); err == nil {
		t.Fatal("parseMCULayouts accepted a garbage line")
	}
}

func TestMCUFeatureEntries(t *testing.T) {
	if got := mcuFeatureEntries([]mcuLayout{testZephyr1}); len(got) != 1 || got[0] != "mcu-flash:zephyr-1" {
		t.Fatalf("mcuFeatureEntries = %v", got)
	}
}

func TestMCUFeatureEntriesWithoutHelper(t *testing.T) {
	layouts, final := probeMCULayouts(filepath.Join(t.TempDir(), "absent"))
	if got := mcuFeatureEntries(layouts); len(got) != 0 || !final {
		t.Fatalf("feature entries without a helper = %v (final %v), want none, final", got, final)
	}
}

func TestCheckLoaderELF(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.elf", testELF(elfSegment{0x08000000, 0x100, 0x100}, elfSegment{0x40036400, 0, 0x208}))
	if err := checkLoaderELF(ok, testZephyr1); err != nil {
		t.Errorf("in-range loader rejected: %v", err)
	}
	bad := write("bad.elf", testELF(elfSegment{0x080d0000, 0x10, 0x10}))
	if err := checkLoaderELF(bad, testZephyr1); err == nil {
		t.Error("loader writing the boot-animation partition accepted")
	}
	notELF := write("x.bin", []byte("not an elf"))
	if err := checkLoaderELF(notELF, testZephyr1); err == nil {
		t.Error("non-ELF loader accepted")
	}
}

func TestFlashFirmware_SpecMustComeFirst(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	reqs := []*agentpbv2.FlashFirmwareRequest{{RequestType: &agentpbv2.FlashFirmwareRequest_Chunk_{
		Chunk: &agentpbv2.FlashFirmwareRequest_Chunk{Data: []byte{1}}}}}
	_, err := runFlash(context.Background(), s, reqs)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("FlashFirmware() = %v, want InvalidArgument", err)
	}
}

func TestFlashFirmware_UnknownLayoutFails(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	in := validFlash()
	in.spec.Layout = "zephyr-2"
	stream, err := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if err != nil {
		t.Fatal(err)
	}
	if f := lastResponse(t, stream).GetFailed(); f == nil || !strings.Contains(f.GetErrorMessage(), "zephyr-2") {
		t.Fatalf("last response = %v, want Failed naming zephyr-2", lastResponse(t, stream))
	}
	if h.callCount() != 0 {
		t.Error("helper ran for an unknown layout")
	}
}

func TestFlashFirmware_HashMismatchNeverRunsHelper(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	in := validFlash()
	in.spec.Sketch.Sha256 = sha([]byte("other"))
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if lastResponse(t, stream).GetFailed() == nil {
		t.Fatalf("last response = %v, want Failed", lastResponse(t, stream))
	}
	if h.callCount() != 0 {
		t.Error("helper ran despite a hash mismatch")
	}
	stagingEmpty(t, s)
}

func TestFlashFirmware_ShortStreamNeverRunsHelper(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	in := validFlash()
	data := append(in.loader, in.sketch...)
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, data[:len(data)-10], 64))
	if lastResponse(t, stream).GetFailed() == nil {
		t.Fatalf("last response = %v, want Failed", lastResponse(t, stream))
	}
	if h.callCount() != 0 {
		t.Error("helper ran on a short stream")
	}
	stagingEmpty(t, s)
}

func TestFlashFirmware_SketchTooLarge(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	in := validFlash()
	in.spec.Sketch.Size = 0xc0000 + 1
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, nil, 64))
	if f := lastResponse(t, stream).GetFailed(); f == nil || !strings.Contains(f.GetErrorMessage(), "sketch") {
		t.Fatalf("last response = %v, want Failed about the sketch size", lastResponse(t, stream))
	}
	if h.callCount() != 0 {
		t.Error("helper ran for an oversized sketch")
	}
}

func TestFlashFirmware_SecondFlashWaitsForTheFirst(t *testing.T) {
	h := &fakeHelper{output: okStatus, started: make(chan struct{}), release: make(chan struct{})}
	s := newTestMcuService(t, h)
	in := validFlash()
	reqs := flashRequests(in.spec, append(in.loader, in.sketch...), 64)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runFlash(context.Background(), s, reqs)
	}()
	<-h.started

	second := make(chan *flashStream)
	go func() {
		stream, _ := runFlash(context.Background(), s, reqs)
		second <- stream
	}()
	time.Sleep(50 * time.Millisecond)
	if h.callCount() != 1 {
		t.Fatalf("helper calls while the first flash runs = %d, want 1", h.callCount())
	}
	close(h.release)
	<-done
	if c := lastResponse(t, <-second).GetCompleted(); c == nil {
		t.Fatal("the second flash did not complete after the first finished")
	}
	if h.callCount() != 2 {
		t.Errorf("helper calls = %d, want 2", h.callCount())
	}
}

func TestFlashFirmware_BusyWhenTheOtherFlashOutlastsTheWait(t *testing.T) {
	h := &fakeHelper{output: okStatus, started: make(chan struct{}), release: make(chan struct{})}
	s := newTestMcuService(t, h)
	s.helperTimeout = 50 * time.Millisecond
	in := validFlash()
	reqs := flashRequests(in.spec, append(in.loader, in.sketch...), 64)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runFlash(context.Background(), s, reqs)
	}()
	<-h.started

	stream, err := runFlash(context.Background(), s, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if f := lastResponse(t, stream).GetFailed(); f == nil || !strings.Contains(f.GetErrorMessage(), "busy") {
		t.Fatalf("second flash response = %v, want Failed containing busy", lastResponse(t, stream))
	}
	close(h.release)
	<-done
	if h.callCount() != 1 {
		t.Errorf("helper calls = %d, want 1", h.callCount())
	}
}

func TestFlashFirmware_HoldsTheUpdaterDuringTheFlash(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	var events []string
	s.holdUpdater = func(*zap.Logger) func() {
		events = append(events, fmt.Sprintf("hold@%d", h.callCount()))
		return func() { events = append(events, fmt.Sprintf("release@%d", h.callCount())) }
	}
	in := validFlash()
	if _, err := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, " "); got != "hold@0 release@1" {
		t.Fatalf("updater events = %q, want hold before the helper and release after", got)
	}
}

func TestFlashFirmware_ClientCancelDoesNotAbortHelper(t *testing.T) {
	h := &fakeHelper{output: okStatus, started: make(chan struct{}), release: make(chan struct{})}
	s := newTestMcuService(t, h)
	in := validFlash()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runFlash(ctx, s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	}()
	<-h.started
	cancel()
	time.Sleep(10 * time.Millisecond)
	if err := h.calls[0].ctx.Err(); err != nil {
		t.Fatalf("helper context ended with the client: %v", err)
	}
	close(h.release)
	<-done
	if _, err := os.Stat(s.statePath); err != nil {
		t.Fatalf("state not written after a client cancel: %v", err)
	}
}

func TestFlashFirmware_CompletedCarriesStatusAndPreviousApp(t *testing.T) {
	h := &fakeHelper{output: "Info : flashing\n" + strings.Replace(okStatus, `"router_active":true`, `"router_active":false`, 1) + "\n"}
	s := newTestMcuService(t, h)
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.statePath, []byte(`{"app_id":"old.app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	in := validFlash()
	stream, err := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if err != nil {
		t.Fatal(err)
	}
	c := lastResponse(t, stream).GetCompleted()
	if c == nil {
		t.Fatalf("last response = %v, want Completed", lastResponse(t, stream))
	}
	if c.GetLoaderWritten() || !c.GetSketchWritten() || c.GetRouterActive() || c.GetPreviousAppId() != "old.app" {
		t.Errorf("Completed = %+v, want {false true false old.app}", c)
	}
	var progress []string
	for _, r := range stream.sent {
		if p := r.GetProgress(); p != nil && p.GetLogLine() != "" {
			progress = append(progress, p.GetLogLine())
		}
	}
	if len(progress) != 1 || progress[0] != "Info : flashing" {
		t.Errorf("progress log lines = %q, want the helper's output line", progress)
	}
	call := h.calls[0].args
	if len(call) != 3 || call[0] != "zephyr-1" {
		t.Errorf("helper args = %q, want layout then two files", call)
	}
	var state map[string]any
	data, _ := os.ReadFile(s.statePath)
	if err := json.Unmarshal(data, &state); err != nil || state["app_id"] != "new.app" || state["sketch_sha256"] != in.spec.Sketch.Sha256 {
		t.Errorf("state = %s (%v), want new.app with the sketch hash", data, err)
	}
	stagingEmpty(t, s)
}

func TestFlashFirmware_HelperFailureReturnsError(t *testing.T) {
	h := &fakeHelper{
		output:  `wendy-mcu-status {"ok":false,"loader_written":false,"sketch_written":false,"router_active":true,"error":"OpenOCD failed (exit 1): Error: init mode failed"}` + "\n",
		waitErr: &exitError{code: 4},
	}
	s := newTestMcuService(t, h)
	in := validFlash()
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	f := lastResponse(t, stream).GetFailed()
	if f == nil || !strings.Contains(f.GetErrorMessage(), "init mode failed") {
		t.Fatalf("last response = %v, want Failed carrying the helper error", lastResponse(t, stream))
	}
	if _, err := os.Stat(s.statePath); err == nil {
		t.Error("state written after a failed flash")
	}
}

type exitError struct{ code int }

func (e *exitError) Error() string { return "exit status " + string(rune('0'+e.code)) }

func TestFlashFirmware_TimeoutSaysTimedOut(t *testing.T) {
	h := &fakeHelper{}
	s := newTestMcuService(t, h)
	s.helperTimeout = 20 * time.Millisecond
	var resumed [][]string
	s.runHelper = func(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
		if args[0] == "--resume" {
			resumed = append(resumed, args)
			return io.NopCloser(strings.NewReader("")), func() error { return nil }, nil
		}
		<-ctx.Done()
		return io.NopCloser(strings.NewReader("")), func() error { return errors.New("signal: killed") }, nil
	}
	in := validFlash()
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	f := lastResponse(t, stream).GetFailed()
	if f == nil || !strings.Contains(f.GetErrorMessage(), "timed out") {
		t.Fatalf("last response = %v, want Failed saying it timed out", lastResponse(t, stream))
	}
	if len(resumed) != 1 || resumed[0][1] != "zephyr-1" {
		t.Fatalf("resume calls = %v, want one for zephyr-1: a killed helper leaves the MCU halted", resumed)
	}
}
