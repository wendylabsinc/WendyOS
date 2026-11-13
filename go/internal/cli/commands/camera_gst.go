package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// gstLaunchFallbackPathsFn indirects gstLaunchFallbackPaths so tests can stub
// the platform-specific candidate list.
var gstLaunchFallbackPathsFn = gstLaunchFallbackPaths

// resolveGSTLaunch locates the gst-launch-1.0 binary used for local camera
// playback. It checks PATH first, then falls back to well-known install
// locations (see gstLaunchFallbackPaths, which is platform-specific). The
// fallback matters on Windows: the GStreamer installer (and the winget
// "gstreamer" package that wraps it) does not add its bin directory to PATH —
// it only sets the GSTREAMER_1_0_ROOT_* environment variables — so a bare
// exec.LookPath would fail even on a correctly installed system.
func resolveGSTLaunch() (string, error) {
	if path, err := exec.LookPath(gstLaunchName); err == nil {
		return path, nil
	}
	for _, candidate := range gstLaunchFallbackPathsFn() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found; install GStreamer or use --stdout to pipe raw video", gstLaunchName)
}

// brewLookPathFn resolves the "brew" binary; indirected so tests can stub it
// without depending on whether Homebrew is actually installed.
var brewLookPathFn = exec.LookPath

// installGStreamerFn runs `brew install gstreamer`; indirected so tests can
// stub it without invoking Homebrew.
var installGStreamerFn = installGStreamerViaBrew

// ensureGSTLaunch resolves gst-launch-1.0 like resolveGSTLaunch, but on an
// interactive macOS session where it's missing, offers to install GStreamer
// via Homebrew first. Non-interactive runs, non-macOS hosts, and hosts
// without Homebrew fall straight through to resolveGSTLaunch's plain "not
// found" error instead of prompting.
func ensureGSTLaunch(ctx context.Context) (string, error) {
	return ensureGSTLaunchForHostOS(ctx, runtime.GOOS)
}

func ensureGSTLaunchForHostOS(ctx context.Context, hostOS string) (string, error) {
	path, err := resolveGSTLaunch()
	if err == nil {
		return path, nil
	}
	if hostOS != "darwin" || !isInteractiveTerminalFn() {
		return "", err
	}
	if _, lookErr := brewLookPathFn("brew"); lookErr != nil {
		return "", err
	}
	if !confirmFn("GStreamer is required to view the camera and was not found. Install it now with `brew install gstreamer`?") {
		return "", err
	}
	if instErr := installGStreamerFn(ctx); instErr != nil {
		if errors.Is(instErr, ErrUserCancelled) {
			return "", instErr
		}
		return "", fmt.Errorf("installing GStreamer via Homebrew: %w", instErr)
	}
	return resolveGSTLaunch()
}

// installGStreamerViaBrew runs `brew install gstreamer` behind a spinner
// instead of streaming Homebrew's own log output; the captured output is
// printed only if the install fails.
func installGStreamerViaBrew(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "brew", "install", "gstreamer")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = nil

	prog := tui.NewProgressProgram(tui.NewSpinner("Installing GStreamer via Homebrew..."))

	var (
		runErr error
		doneCh = make(chan struct{})
	)
	go func() {
		defer close(doneCh)
		runErr = cmd.Run()
		// Keep spinner teardown quiet; callers handle the returned error/output.
		prog.Send(tui.SpinnerDoneMsg{})
	}()

	finalModel, err := tui.RunProgram(prog)
	if err != nil {
		cancel()
		<-doneCh
		return fmt.Errorf("spinner TUI: %w", err)
	}

	sm, ok := finalModel.(tui.SpinnerModel)
	if !ok {
		cancel()
		<-doneCh
		return fmt.Errorf("spinner TUI: unexpected model type %T", finalModel)
	}
	if !sm.Done() {
		cancel()
		<-doneCh
		return ErrUserCancelled
	}

	<-doneCh
	if runErr != nil {
		_, _ = os.Stderr.Write(out.Bytes())
		return runErr
	}
	return nil
}
