//go:build darwin || linux

package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ebitengine/purego"
)

// GStreamer state and message constants from gst/gstelement.h and
// gst/gstmessage.h. They are ABI-stable across GStreamer 1.x.
const (
	gstStateNull           = 1
	gstStatePlaying        = 4
	gstStateChangeFailure  = 0
	gstMessageEOS          = 1 << 0
	gstMessageError        = 1 << 1
	gstreamerPipelineFDArg = "fd=1"
)

// gstreamerAPI is the small, stable GStreamer surface needed by the IP-camera
// depayload pipeline. Loading it dynamically keeps the agent build CGO-free and
// preserves the existing runtime dependency model: devices without GStreamer
// get an actionable error rather than a link-time failure.
type gstreamerAPI struct {
	init                func(uintptr, uintptr)
	parseLaunch         func(string, *uintptr) uintptr
	elementSetState     func(uintptr, int32) int32
	elementGetBus       func(uintptr) uintptr
	busTimedPopFiltered func(uintptr, uint64, uint32) uintptr
	busSetFlushing      func(uintptr, int32)
	miniObjectUnref     func(uintptr)
	objectUnref         func(uintptr)
	errorFree           func(uintptr)
}

var (
	gstreamerOnce sync.Once
	gstreamer     *gstreamerAPI
	gstreamerErr  error
)

// loadGStreamer loads and initializes GStreamer once for the process. Library
// handles intentionally remain open for the agent lifetime because registered
// function pointers and active pipelines may outlive any individual stream.
func loadGStreamer() (*gstreamerAPI, error) {
	gstreamerOnce.Do(func() {
		gstHandle, err := openGStreamerLibrary(gstreamerLibraryNames())
		if err != nil {
			gstreamerErr = errors.New("GStreamer library not found; install GStreamer on the device")
			return
		}
		gobjectHandle, err := openGStreamerLibrary(gobjectLibraryNames())
		if err != nil {
			gstreamerErr = errors.New("GObject library not found; install GStreamer on the device")
			return
		}
		glibHandle, err := openGStreamerLibrary(glibLibraryNames())
		if err != nil {
			gstreamerErr = errors.New("GLib library not found; install GStreamer on the device")
			return
		}

		api := &gstreamerAPI{}
		purego.RegisterLibFunc(&api.init, gstHandle, "gst_init")
		purego.RegisterLibFunc(&api.parseLaunch, gstHandle, "gst_parse_launch")
		purego.RegisterLibFunc(&api.elementSetState, gstHandle, "gst_element_set_state")
		purego.RegisterLibFunc(&api.elementGetBus, gstHandle, "gst_element_get_bus")
		purego.RegisterLibFunc(&api.busTimedPopFiltered, gstHandle, "gst_bus_timed_pop_filtered")
		purego.RegisterLibFunc(&api.busSetFlushing, gstHandle, "gst_bus_set_flushing")
		purego.RegisterLibFunc(&api.miniObjectUnref, gstHandle, "gst_mini_object_unref")
		purego.RegisterLibFunc(&api.objectUnref, gobjectHandle, "g_object_unref")
		purego.RegisterLibFunc(&api.errorFree, glibHandle, "g_error_free")

		api.init(0, 0)
		gstreamer = api
	})
	return gstreamer, gstreamerErr
}

func openGStreamerLibrary(names []string) (uintptr, error) {
	var lastErr error
	for _, name := range names {
		handle, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil {
			return handle, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

func gstreamerLibraryNames() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"libgstreamer-1.0.0.dylib",
			"/opt/homebrew/lib/libgstreamer-1.0.0.dylib",
			"/usr/local/lib/libgstreamer-1.0.0.dylib",
		}
	}
	return []string{"libgstreamer-1.0.so.0"}
}

func gobjectLibraryNames() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"libgobject-2.0.0.dylib",
			"/opt/homebrew/lib/libgobject-2.0.0.dylib",
			"/usr/local/lib/libgobject-2.0.0.dylib",
		}
	}
	return []string{"libgobject-2.0.so.0"}
}

func glibLibraryNames() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"libglib-2.0.0.dylib",
			"/opt/homebrew/lib/libglib-2.0.0.dylib",
			"/usr/local/lib/libglib-2.0.0.dylib",
		}
	}
	return []string{"libglib-2.0.so.0"}
}

// gstreamerPipelineDescription redirects the fdsink from stdout to the private
// pipe used by the in-process runner, when one is present. Sink-terminated
// pipelines — Task C3's v4l2loopback pump, for example, where the device node
// is the sink rather than an fdsink — carry no fd=1 token at all, so hasFD
// reports whether a rewrite happened instead of treating its absence as an
// error. Tokens come from ipcam.PipelineArgs / ipcam.LoopbackPipelineArgs; the
// credential-bearing URL is already percent-encoded and contains no
// whitespace.
func gstreamerPipelineDescription(args []string, fd uintptr) (string, bool, error) {
	parts := append([]string(nil), args...)
	hasFD := false
	for i, part := range parts {
		if part == gstreamerPipelineFDArg {
			parts[i] = fmt.Sprintf("fd=%d", fd)
			hasFD = true
		}
	}
	return strings.Join(parts, " "), hasFD, nil
}

// RunIPCameraGStreamerHelper reads one JSON-encoded pipeline from in and writes
// its byte stream to out. It runs in a short-lived helper process so camera
// credentials never enter argv while GStreamer plugin failures remain isolated
// from the long-running agent.
func RunIPCameraGStreamerHelper(in io.Reader, out io.Writer) error {
	// GStreamer initialization on macOS must happen on the process's initial OS
	// thread. handleUtilityCommand invokes this before the agent starts any other
	// work, so locking here preserves that requirement.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var args []string
	decoder := json.NewDecoder(io.LimitReader(in, 1024*1024))
	if err := decoder.Decode(&args); err != nil {
		return errors.New("reading GStreamer pipeline failed")
	}
	return runIPCameraGStreamerPipeline(context.Background(), args, func(chunk []byte) error {
		_, err := out.Write(chunk)
		return err
	})
}

// runIPCameraGStreamerPipeline runs the depayload pipeline through
// libgstreamer. Pipeline failures are deliberately generic because GStreamer
// diagnostics may repeat credential-bearing property values.
func runIPCameraGStreamerPipeline(ctx context.Context, args []string, emit func([]byte) error) error {
	api, err := loadGStreamer()
	if err != nil {
		return err
	}

	// Only allocate the private capture pipe when the pipeline actually has an
	// fdsink to redirect. Sink-terminated pipelines — the v4l2loopback pump, for
	// example — carry no fd=1 token, and a pipe neither of them writes to nor
	// reads from would just be two file descriptors held open for nothing.
	var reader, writer *os.File
	var fd uintptr
	for _, part := range args {
		if part == gstreamerPipelineFDArg {
			reader, writer, err = os.Pipe()
			if err != nil {
				return fmt.Errorf("creating capture pipe: %w", err)
			}
			fd = writer.Fd()
			break
		}
	}
	closePipe := func() {
		if writer != nil {
			writer.Close() //nolint:errcheck
		}
		if reader != nil {
			reader.Close() //nolint:errcheck
		}
	}
	defer closePipe()

	description, hasFD, err := gstreamerPipelineDescription(args, fd)
	if err != nil {
		closePipe()
		return err
	}
	var parseErr uintptr
	pipeline := api.parseLaunch(description, &parseErr)
	if parseErr != 0 {
		api.errorFree(parseErr)
	}
	if pipeline == 0 {
		closePipe()
		return errors.New("creating GStreamer capture pipeline failed")
	}
	defer api.objectUnref(pipeline)

	bus := api.elementGetBus(pipeline)
	if bus == 0 {
		closePipe()
		return errors.New("creating GStreamer event bus failed")
	}
	defer api.objectUnref(bus)

	if api.elementSetState(pipeline, gstStatePlaying) == gstStateChangeFailure {
		api.elementSetState(pipeline, gstStateNull)
		closePipe()
		return errors.New("starting GStreamer capture pipeline failed")
	}

	terminal := make(chan struct{})
	pollCtx, cancelPoll := context.WithCancel(ctx)
	go func() {
		defer close(terminal)
		for pollCtx.Err() == nil {
			message := api.busTimedPopFiltered(bus, uint64(100*time.Millisecond), gstMessageEOS|gstMessageError)
			if message != 0 {
				api.miniObjectUnref(message)
				return
			}
		}
	}()
	// Wait for the bounded bus poll to exit before releasing the bus.
	defer func() { <-terminal }()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancelPoll()
			api.elementSetState(pipeline, gstStateNull)
			api.busSetFlushing(bus, 1)
			// Leave the reader open so buffered output drains before EOF.
			if writer != nil {
				writer.Close() //nolint:errcheck
			}
		})
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-terminal:
		}
		stop()
		close(stopped)
	}()

	if hasFD {
		buf := make([]byte, 64*1024)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if err := emit(chunk); err != nil {
					stop()
					<-stopped
					return errors.New("writing GStreamer output failed")
				}
			}
			if readErr != nil {
				break
			}
		}
	} else {
		// No stdout to pump: the sink is the v4l2loopback node itself, so there is
		// nothing to read. Just wait for the pipeline to reach a terminal state or
		// for the context to cancel, exactly as the fd path does once its read
		// loop ends.
		<-stopped
	}
	stop()
	<-stopped
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("GStreamer capture pipeline stopped unexpectedly")
}
