package framesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// fakeHelperEnv turns this test binary into a capture helper when it is set, so
// the exec path — a real process, a real pipe, a real exit — is covered without
// shipping a second binary or owning a camera.
const fakeHelperEnv = "WENDY_TEST_FAKE_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeHelperEnv); mode != "" {
		os.Exit(runFakeHelper(mode))
	}
	os.Exit(m.Run())
}

func runFakeHelper(mode string) int {
	switch mode {
	case "describe":
		for _, serial := range []string{"111", "222"} {
			if err := WriteRecord(os.Stdout, RecordSource, &agentpbv2.CalibratedSource{
				Source: "realsense:" + serial, Kind: KindRealSense, Available: true,
				ColourWidth: 640, ColourHeight: 480,
				Provides: []agentpbv2.FrameRequirement{
					agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH,
				},
			}); err != nil {
				return 1
			}
		}
		return 0
	case "stream":
		if err := WriteRecord(os.Stdout, RecordSource, &agentpbv2.CalibratedSource{
			Source: "realsense:111", Kind: KindRealSense, Available: true,
			ColourWidth: 8, ColourHeight: 4,
		}); err != nil {
			return 1
		}
		for i := uint64(1); i <= 2; i++ {
			if err := WriteRecord(os.Stdout, RecordFrame, &agentpbv2.CalibratedFrame{FrameId: i}); err != nil {
				return 1
			}
		}
		return 0
	case "fail":
		// The line that names the cause, then the generic one a dying helper
		// tends to print last. Both must survive into the exit error.
		fmt.Fprintln(os.Stderr, "failed to set power state")
		fmt.Fprint(os.Stderr, "exiting") // no trailing newline, like a crash
		return 1
	default:
		return 2
	}
}

func execLauncherForMode(t *testing.T, mode string) ExecLauncher {
	t.Helper()
	t.Setenv(fakeHelperEnv, mode)
	return ExecLauncher{Path: os.Args[0], Logger: zap.NewNop()}
}

// --- finding the helper ---

// A missing helper is the common case on a stock image, so its error is a
// user-facing message: it must say what to install and where to put it.
func TestFindHelper_NamesWhatIsMissingAndEveryPlaceItLooked(t *testing.T) {
	t.Setenv(HelperEnvOverride, "")
	_, err := FindHelper("wendy-definitely-not-installed")
	if !errors.Is(err, ErrHelperNotInstalled) {
		t.Fatalf("err = %v, want ErrHelperNotInstalled", err)
	}
	for _, dir := range helperSearchDirs {
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error does not mention %s: %v", dir, err)
		}
	}
	if !strings.Contains(err.Error(), HelperEnvOverride) {
		t.Errorf("error does not mention the override: %v", err)
	}
}

func TestFindHelper_HonoursTheEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(HelperEnvOverride, path)
	got, err := FindHelper(HelperName)
	if err != nil || got != path {
		t.Fatalf("FindHelper = %q, %v; want %q", got, err, path)
	}
}

func TestFindHelper_OverrideThatIsNotExecutableIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(HelperEnvOverride, path)
	if _, err := FindHelper(HelperName); err == nil {
		t.Error("a non-executable override was accepted")
	}
}

// --- the wire, over an in-memory helper ---

// memoryLauncher answers with a canned record stream, so the agent side of the
// protocol is tested without spawning anything.
type memoryLauncher struct {
	records []byte
	err     error
	waitErr error
	started int
	args    []string
}

func (l *memoryLauncher) Binary() string { return "memory" }

func (l *memoryLauncher) Start(_ context.Context, args []string) (*HelperRun, error) {
	l.started++
	l.args = args
	if l.err != nil {
		return nil, l.err
	}
	return &HelperRun{
		Records: io.NopCloser(bytes.NewReader(l.records)),
		Wait:    func() error { return l.waitErr },
		Stop:    func() {},
	}, nil
}

func recordBytes(t *testing.T, write func(io.Writer) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHelperSource_ReadsTheNegotiatedDescriptorBeforeAnyFrame(t *testing.T) {
	records := recordBytes(t, func(w io.Writer) error {
		// The helper had to open a mode WITHOUT depth. The listing promised
		// depth; the capture did not deliver it, and the descriptor says so
		// before a single frame is produced.
		if err := WriteRecord(w, RecordSource, &agentpbv2.CalibratedSource{
			Source: "realsense:111", ColourWidth: 8, ColourHeight: 4, Available: true,
		}); err != nil {
			return err
		}
		return WriteRecord(w, RecordFrame, &agentpbv2.CalibratedFrame{FrameId: 1})
	})
	launcher := &memoryLauncher{records: records}
	src := NewHelperSource(&agentpbv2.CalibratedSource{
		Source: "realsense:111", Kind: KindRealSense, Available: true,
		Provides: []agentpbv2.FrameRequirement{alignedDepth},
	}, launcher, zap.NewNop())

	stream, err := src.Open(context.Background(), Options{Width: 8, Height: 4, Framerate: 15})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	negotiated := stream.(interface {
		Negotiated() *agentpbv2.CalibratedSource
	}).Negotiated()
	if len(negotiated.GetProvides()) != 0 {
		t.Errorf("negotiated capture claims %v; the listing's promise must not leak into it",
			Slugs(negotiated.GetProvides()))
	}
	// The geometry reached the helper as flags.
	joined := strings.Join(launcher.args, " ")
	for _, want := range []string{"stream", "--source realsense:111", "--width 8", "--height 4", "--fps 15"} {
		if !strings.Contains(joined, want) {
			t.Errorf("helper args %q do not contain %q", joined, want)
		}
	}

	f, err := stream.Next(context.Background())
	if err != nil || f.GetFrameId() != 1 {
		t.Fatalf("Next = %+v, %v", f, err)
	}
	// A frame the helper did not stamp inherits the source it came from, so a
	// consumer never has to correlate it by hand.
	if f.GetSource() != "realsense:111" {
		t.Errorf("frame source = %q", f.GetSource())
	}
}

func TestHelperSource_HelperThatDiesBeforeDescribingSaysWhy(t *testing.T) {
	launcher := &memoryLauncher{records: nil, waitErr: errors.New("could not claim USB device")}
	src := NewHelperSource(&agentpbv2.CalibratedSource{Source: "realsense:111", Available: true},
		launcher, zap.NewNop())
	_, err := src.Open(context.Background(), Options{})
	if err == nil {
		t.Fatal("Open succeeded on a helper that produced nothing")
	}
	if !strings.Contains(err.Error(), "could not claim USB device") {
		t.Errorf("error loses the helper's own reason: %v", err)
	}
}

// --- a real process ---

func TestExecLauncher_RunsARealHelperAndReadsItsRecords(t *testing.T) {
	launcher := execLauncherForMode(t, "stream")
	src := NewHelperSource(&agentpbv2.CalibratedSource{Source: "realsense:111", Available: true},
		launcher, zap.NewNop())

	stream, err := src.Open(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	for i := uint64(1); i <= 2; i++ {
		f, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if f.GetFrameId() != i {
			t.Errorf("frame id = %d, want %d", f.GetFrameId(), i)
		}
	}
	// The helper exits; a clean end is io.EOF, not an error.
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("end of helper = %v, want io.EOF", err)
	}
}

// An exit status alone throws away the only thing that says what went wrong:
// librealsense has already printed it on stderr by then.
func TestExecLauncher_FoldsTheHelpersStderrIntoItsExitError(t *testing.T) {
	launcher := execLauncherForMode(t, "fail")
	run, err := launcher.Start(context.Background(), []string{"stream"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _ = io.Copy(io.Discard, run.Records)
	err = run.Wait()
	if err == nil {
		t.Fatal("a helper that exited 1 reported no error")
	}
	for _, want := range []string{"failed to set power state", "exiting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not carry %q from the helper's stderr: %v", want, err)
		}
	}
}

func TestTailBuffer_KeepsEveryRetainedLineNotJustTheLast(t *testing.T) {
	tail := &tailBuffer{limit: 1 << 10}
	// Written the way a pipe delivers it: in pieces that do not respect lines.
	for _, chunk := range []string{"failed to claim ", "USB interface\nexit", "ing"} {
		if _, err := tail.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tail.String(); got != "failed to claim USB interface; exiting" {
		t.Errorf("tail = %q", got)
	}
}

func TestTailBuffer_StaysWithinItsBudgetAndDropsTheOldest(t *testing.T) {
	tail := &tailBuffer{limit: 24}
	for _, line := range []string{"first line", "second line", "third line"} {
		_, _ = tail.Write([]byte(line + "\n"))
	}
	got := tail.String()
	if strings.Contains(got, "first") || !strings.Contains(got, "third") {
		t.Errorf("tail = %q; the oldest line goes, the newest stays", got)
	}
	// A helper that never writes a newline is bounded too.
	spin := &tailBuffer{limit: 16}
	_, _ = spin.Write([]byte(strings.Repeat("x", 100)))
	if n := len(spin.String()); n > 2*16 {
		t.Errorf("an unterminated line grew to %d bytes against a 16-byte budget", n)
	}
}

// --- where the helper is looked for ---

func withHelperSearchDirs(t *testing.T, dirs ...string) {
	t.Helper()
	prev := helperSearchDirs
	helperSearchDirs = dirs
	t.Cleanup(func() { helperSearchDirs = prev })
}

func writeHelper(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, HelperName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to the umask; the mode under test is set explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// The image installs the helper in libexec. A copy left on the daemon's PATH
// -- a developer build, an older package -- must not shadow it.
func TestFindHelper_PrefersTheInstalledDirsOverPATH(t *testing.T) {
	t.Setenv(HelperEnvOverride, "")
	installed := writeHelper(t, t.TempDir(), 0o755)
	onPath := writeHelper(t, t.TempDir(), 0o755)
	withHelperSearchDirs(t, filepath.Dir(installed))
	t.Setenv("PATH", filepath.Dir(onPath))

	got, err := FindHelper(HelperName)
	if err != nil || got != installed {
		t.Fatalf("FindHelper = %q, %v; want the installed copy %q over the one on PATH", got, err, installed)
	}
}

func TestFindHelper_FallsBackToPATHWhenNothingIsInstalled(t *testing.T) {
	t.Setenv(HelperEnvOverride, "")
	onPath := writeHelper(t, t.TempDir(), 0o755)
	withHelperSearchDirs(t, filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PATH", filepath.Dir(onPath))

	got, err := FindHelper(HelperName)
	if err != nil || got != onPath {
		t.Fatalf("FindHelper = %q, %v; want %q", got, err, onPath)
	}
}

// The agent runs as root. A helper anyone can rewrite is a way to run
// anything as root with the camera in hand.
func TestFindHelper_RefusesAWorldWritableHelper(t *testing.T) {
	t.Setenv(HelperEnvOverride, "")
	writable := writeHelper(t, t.TempDir(), 0o777)
	withHelperSearchDirs(t, filepath.Dir(writable))
	t.Setenv("PATH", "")
	if got, err := FindHelper(HelperName); err == nil {
		t.Errorf("a world-writable helper was accepted: %q", got)
	}
	t.Setenv(HelperEnvOverride, writable)
	if got, err := FindHelper(HelperName); err == nil {
		t.Errorf("a world-writable helper was accepted through the override: %q", got)
	}
}

func TestDescribeThroughHelper_CollectsEveryCamera(t *testing.T) {
	launcher := execLauncherForMode(t, "describe")
	got, err := describeThroughHelper(context.Background(), launcher)
	if err != nil {
		t.Fatalf("describeThroughHelper: %v", err)
	}
	if len(got) != 2 || got[0].GetSource() != "realsense:111" || got[1].GetSource() != "realsense:222" {
		t.Fatalf("sources = %+v", got)
	}
}

// --- present but unusable ---

func TestUnavailableSource_DescribesItselfAndRefusesToOpen(t *testing.T) {
	src := NewUnavailableSource("realsense", KindRealSense, "D435i", "the helper is not installed")
	desc, err := src.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.GetAvailable() || desc.GetUnavailableReason() == "" {
		t.Errorf("descriptor = %+v; an unusable source must say so and say why", desc)
	}
	if _, err := src.Open(context.Background(), Options{}); err == nil {
		t.Error("an unavailable source opened")
	}
}
