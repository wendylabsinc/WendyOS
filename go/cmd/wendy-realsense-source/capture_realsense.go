//go:build linux && cgo && realsense

package main

// The librealsense binding: the one piece of this feature that cannot be
// written without the vendor SDK, kept in the one file that needs it.
//
// It does four things the agent cannot do for itself, in this order, because
// each one is only available here:
//
//  1. opens colour and depth as ONE pipeline, so both planes come from one
//     frame set with one frame number — pairing is done at the sensor, not
//     guessed afterwards from two timestamps;
//  2. runs rs2_align(RS2_STREAM_COLOR) over that frame set, using the module's
//     factory extrinsics, which exist in no other process;
//  3. reads rs2_get_depth_scale, without which the depth plane is uint16 in
//     unknown units — the failure this whole feature exists to prevent;
//  4. reads the colour stream's factory intrinsics, and reports them as
//     MEASURED so a consumer computing a physical size can tell them from a
//     datasheet guess.
//
// BUILD: opt-in, with `-tags realsense` on a host carrying librealsense2's
// development headers (pkg-config name `realsense2`). PR CI builds without the
// tag and gets capture_unsupported.go, so no WendyOS image or developer
// machine needs a C++ SDK to compile this repository.

/*
#cgo pkg-config: realsense2
#include <stdlib.h>
#include <librealsense2/rs.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// defaultColour is the mode opened when a subscriber asks for no geometry.
// 640x480 at 30 fps is supported by every D4xx and fits both planes inside a
// default 4 MiB gRPC message with room to spare, so the common request needs no
// client-side limit raising at all.
const (
	defaultWidth     = 640
	defaultHeight    = 480
	defaultFramerate = 30

	// waitTimeoutMs bounds one rs2_pipeline_wait_for_frames. Long enough for a
	// USB re-enumeration hiccup, short enough that a camera that has actually
	// gone away ends the stream rather than hanging a subscriber forever.
	waitTimeoutMs = 5000
)

// rsError turns librealsense's out-parameter error into a Go error and frees
// it. Every rs2_ call takes one, and ignoring it is how a failed call turns
// into a segfault two lines later.
func rsError(e *C.rs2_error) error {
	if e == nil {
		return nil
	}
	msg := C.GoString(C.rs2_get_error_message(e))
	fn := C.GoString(C.rs2_get_failed_function(e))
	C.rs2_free_error(e)
	if fn != "" {
		return fmt.Errorf("librealsense %s: %s", fn, msg)
	}
	return errors.New("librealsense: " + msg)
}

// realSenseCapturer owns the librealsense context.
type realSenseCapturer struct {
	ctx *C.rs2_context
}

func newCapturer() (capturer, error) {
	var e *C.rs2_error
	ctx := C.rs2_create_context(C.RS2_API_VERSION, &e)
	if err := rsError(e); err != nil {
		return nil, err
	}
	return &realSenseCapturer{ctx: ctx}, nil
}

func (c *realSenseCapturer) Close() error {
	if c.ctx != nil {
		C.rs2_delete_context(c.ctx)
		c.ctx = nil
	}
	return nil
}

// deviceInfo reads one string property, tolerating a device that does not carry
// it: a serial number is how a source is named, but an older firmware missing a
// USB descriptor must not take the whole enumeration down with it.
func deviceInfo(dev *C.rs2_device, info C.rs2_camera_info) string {
	var e *C.rs2_error
	if C.rs2_supports_device_info(dev, info, &e) == 0 {
		_ = rsError(e)
		return ""
	}
	if err := rsError(e); err != nil {
		return ""
	}
	s := C.rs2_get_device_info(dev, info, &e)
	if err := rsError(e); err != nil {
		return ""
	}
	return C.GoString(s)
}

// SourceName is the stable identity a calibrated source is addressed by. The
// serial number, because it is the only thing about a RealSense that survives a
// reboot, a replug and a move to another port — the same durability argument
// the V4L2 stable ids make (go/internal/agent/services/video_stable_id.go).
func SourceName(serial string) string { return framesource.KindRealSense + ":" + serial }

func (c *realSenseCapturer) Describe(context.Context) ([]*agentpbv2.CalibratedSource, error) {
	var e *C.rs2_error
	list := C.rs2_query_devices(c.ctx, &e)
	if err := rsError(e); err != nil {
		return nil, err
	}
	defer C.rs2_delete_device_list(list)

	count := int(C.rs2_get_device_count(list, &e))
	if err := rsError(e); err != nil {
		return nil, err
	}
	out := make([]*agentpbv2.CalibratedSource, 0, count)
	for i := 0; i < count; i++ {
		dev := C.rs2_create_device(list, C.int(i), &e)
		if err := rsError(e); err != nil {
			return nil, err
		}
		serial := deviceInfo(dev, C.RS2_CAMERA_INFO_SERIAL_NUMBER)
		name := deviceInfo(dev, C.RS2_CAMERA_INFO_NAME)
		hasDepth := deviceHasDepthSensor(dev)
		C.rs2_delete_device(dev)
		if serial == "" {
			// Without a serial there is no identity that survives a replug, and
			// an unstable name is worse than no entry: a consumer would pin it
			// and silently address a different camera tomorrow.
			continue
		}

		// Everything a D4xx can promise, it promises: the module carries
		// factory extrinsics (so depth aligns), a factory depth scale, and a
		// colour sensor whose applied exposure and gain read back.
		provides := []agentpbv2.FrameRequirement{
			agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS,
			agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS,
		}
		src := &agentpbv2.CalibratedSource{
			Source:       SourceName(serial),
			Kind:         framesource.KindRealSense,
			Description:  name,
			ColourWidth:  defaultWidth,
			ColourHeight: defaultHeight,
			ColourFourcc: colourFourcc,
			Available:    true,
		}
		if hasDepth {
			provides = append([]agentpbv2.FrameRequirement{
				agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH,
			}, provides...)
			src.DepthWidth, src.DepthHeight = defaultWidth, defaultHeight
		}
		src.Provides = provides
		src.MaxFrameBytes = framesource.FrameBytes(
			defaultWidth*colourBytesPerPixel, defaultHeight,
			src.GetDepthWidth()*2, src.GetDepthHeight())
		out = append(out, src)
	}
	return out, nil
}

// colourFourcc/colourBytesPerPixel: BGR8 is requested rather than YUYV because
// every consumer of a calibrated frame is an analytic one, and handing it
// interleaved chroma to unpack would put a colour conversion in each of them.
const (
	colourFourcc        = "BGR3" // V4L2's fourcc for packed 24-bit BGR
	colourBytesPerPixel = 3
)

func deviceHasDepthSensor(dev *C.rs2_device) bool {
	var e *C.rs2_error
	sensors := C.rs2_query_sensors(dev, &e)
	if err := rsError(e); err != nil {
		return false
	}
	defer C.rs2_delete_sensor_list(sensors)
	count := int(C.rs2_get_sensors_count(sensors, &e))
	if err := rsError(e); err != nil {
		return false
	}
	for i := 0; i < count; i++ {
		s := C.rs2_create_sensor(sensors, C.int(i), &e)
		if err := rsError(e); err != nil {
			continue
		}
		extendable := C.rs2_is_sensor_extendable_to(s, C.RS2_EXTENSION_DEPTH_SENSOR, &e) != 0
		_ = rsError(e)
		C.rs2_delete_sensor(s)
		if extendable {
			return true
		}
	}
	return false
}

// --- capture ---

type realSenseCapture struct {
	pipe    *C.rs2_pipeline
	config  *C.rs2_config
	profile *C.rs2_pipeline_profile
	align   *C.rs2_processing_block
	queue   *C.rs2_frame_queue

	desc      *agentpbv2.CalibratedSource
	depthScal float32
	source    string
	frameNo   uint64
}

func (c *realSenseCapturer) Open(_ context.Context, opts CaptureOptions) (capture, error) {
	width, height, fps := opts.Width, opts.Height, opts.Framerate
	if width == 0 || height == 0 {
		width, height = defaultWidth, defaultHeight
	}
	if fps == 0 {
		fps = defaultFramerate
	}
	serial := opts.Source
	if len(serial) > len(framesource.KindRealSense)+1 && serial[:len(framesource.KindRealSense)+1] == framesource.KindRealSense+":" {
		serial = serial[len(framesource.KindRealSense)+1:]
	}

	var e *C.rs2_error
	pipe := C.rs2_create_pipeline(c.ctx, &e)
	if err := rsError(e); err != nil {
		return nil, err
	}
	cfg := C.rs2_create_config(&e)
	if err := rsError(e); err != nil {
		C.rs2_delete_pipeline(pipe)
		return nil, err
	}
	cc := &realSenseCapture{pipe: pipe, config: cfg, source: opts.Source}
	ok := false
	defer func() {
		if !ok {
			cc.Close() //nolint:errcheck
		}
	}()

	if serial != "" {
		cs := C.CString(serial)
		C.rs2_config_enable_device(cfg, cs, &e)
		C.free(unsafe.Pointer(cs))
		if err := rsError(e); err != nil {
			return nil, fmt.Errorf("selecting RealSense %s: %w", serial, err)
		}
	}
	C.rs2_config_enable_stream(cfg, C.RS2_STREAM_COLOR, 0,
		C.int(width), C.int(height), C.RS2_FORMAT_BGR8, C.int(fps), &e)
	if err := rsError(e); err != nil {
		return nil, fmt.Errorf("enabling colour at %dx%d@%d: %w", width, height, fps, err)
	}
	C.rs2_config_enable_stream(cfg, C.RS2_STREAM_DEPTH, 0,
		C.int(width), C.int(height), C.RS2_FORMAT_Z16, C.int(fps), &e)
	if err := rsError(e); err != nil {
		return nil, fmt.Errorf("enabling depth at %dx%d@%d: %w", width, height, fps, err)
	}

	cc.profile = C.rs2_pipeline_start_with_config(pipe, cfg, &e)
	if err := rsError(e); err != nil {
		return nil, fmt.Errorf("starting RealSense capture at %dx%d@%d: %w", width, height, fps, err)
	}

	// The depth scale is a per-device constant, so it is read once here rather
	// than per frame. A capture whose scale cannot be read sends NO depth
	// plane: unknown units are not metres, and defaulting to 1 mm is the
	// silent degradation this file exists to avoid.
	scale, scaleErr := depthScaleOf(cc.profile)
	if scaleErr != nil {
		return nil, fmt.Errorf("reading the depth scale: %w", scaleErr)
	}
	cc.depthScal = scale

	cc.align = C.rs2_create_align(C.RS2_STREAM_COLOR, &e)
	if err := rsError(e); err != nil {
		return nil, fmt.Errorf("creating the depth-to-colour alignment: %w", err)
	}
	// A one-deep queue IS the latest-wins policy at the capture end: the
	// alignment block drops what nobody collected rather than building a
	// backlog of frames that describe a moment already gone.
	cc.queue = C.rs2_create_frame_queue(1, &e)
	if err := rsError(e); err != nil {
		return nil, fmt.Errorf("creating the alignment queue: %w", err)
	}
	C.rs2_start_processing_queue(cc.align, cc.queue, &e)
	if err := rsError(e); err != nil {
		return nil, fmt.Errorf("starting the alignment block: %w", err)
	}

	cc.desc = &agentpbv2.CalibratedSource{
		Source:       opts.Source,
		Kind:         framesource.KindRealSense,
		ColourWidth:  width,
		ColourHeight: height,
		ColourFourcc: colourFourcc,
		DepthWidth:   width,
		DepthHeight:  height,
		Available:    true,
		Provides: []agentpbv2.FrameRequirement{
			agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH,
			agentpbv2.FrameRequirement_FRAME_REQUIREMENT_MEASURED_INTRINSICS,
			agentpbv2.FrameRequirement_FRAME_REQUIREMENT_CAPTURE_SETTINGS,
		},
		MaxFrameBytes: framesource.FrameBytes(
			width*colourBytesPerPixel, height, width*2, height),
	}
	ok = true
	return cc, nil
}

// depthScaleOf finds the depth sensor behind a running pipeline and reads its
// metres-per-unit constant.
func depthScaleOf(profile *C.rs2_pipeline_profile) (float32, error) {
	var e *C.rs2_error
	dev := C.rs2_pipeline_profile_get_device(profile, &e)
	if err := rsError(e); err != nil {
		return 0, err
	}
	defer C.rs2_delete_device(dev)
	sensors := C.rs2_query_sensors(dev, &e)
	if err := rsError(e); err != nil {
		return 0, err
	}
	defer C.rs2_delete_sensor_list(sensors)
	count := int(C.rs2_get_sensors_count(sensors, &e))
	if err := rsError(e); err != nil {
		return 0, err
	}
	for i := 0; i < count; i++ {
		s := C.rs2_create_sensor(sensors, C.int(i), &e)
		if err := rsError(e); err != nil {
			continue
		}
		isDepth := C.rs2_is_sensor_extendable_to(s, C.RS2_EXTENSION_DEPTH_SENSOR, &e) != 0
		if err := rsError(e); err != nil || !isDepth {
			C.rs2_delete_sensor(s)
			continue
		}
		scale := float32(C.rs2_get_depth_scale(s, &e))
		err := rsError(e)
		C.rs2_delete_sensor(s)
		if err != nil {
			return 0, err
		}
		if scale <= 0 {
			return 0, fmt.Errorf("device reported a depth scale of %v", scale)
		}
		return scale, nil
	}
	return 0, errors.New("no depth sensor on this device")
}

func (c *realSenseCapture) Descriptor() *agentpbv2.CalibratedSource { return c.desc }

func (c *realSenseCapture) Close() error {
	var e *C.rs2_error
	if c.queue != nil {
		C.rs2_delete_frame_queue(c.queue)
		c.queue = nil
	}
	if c.align != nil {
		C.rs2_delete_processing_block(c.align)
		c.align = nil
	}
	if c.profile != nil {
		C.rs2_pipeline_stop(c.pipe, &e)
		_ = rsError(e)
		C.rs2_delete_pipeline_profile(c.profile)
		c.profile = nil
	}
	if c.config != nil {
		C.rs2_delete_config(c.config)
		c.config = nil
	}
	if c.pipe != nil {
		C.rs2_delete_pipeline(c.pipe)
		c.pipe = nil
	}
	return nil
}

// Next waits for one frame set, aligns it to the colour stream and renders it
// as a calibrated frame. Both planes come out of the SAME aligned composite, so
// they share a frame id and a capture instant by construction rather than by
// convention.
func (c *realSenseCapture) Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var e *C.rs2_error
	frames := C.rs2_pipeline_wait_for_frames(c.pipe, C.uint(waitTimeoutMs), &e)
	if err := rsError(e); err != nil {
		return nil, err
	}
	// rs2_process_frame consumes its reference, and the composite is still
	// needed for nothing else — but taking the ref explicitly keeps the
	// ownership obvious to the next person to read this.
	C.rs2_process_frame(c.align, frames, &e)
	if err := rsError(e); err != nil {
		return nil, err
	}
	aligned := C.rs2_wait_for_frame(c.queue, C.uint(waitTimeoutMs), &e)
	if err := rsError(e); err != nil {
		return nil, err
	}
	defer C.rs2_release_frame(aligned)

	count := int(C.rs2_embedded_frames_count(aligned, &e))
	if err := rsError(e); err != nil {
		return nil, err
	}

	frame := &agentpbv2.CalibratedFrame{Source: c.source}
	var sawColour, sawDepth bool
	for i := 0; i < count; i++ {
		f := C.rs2_extract_frame(aligned, C.int(i), &e)
		if err := rsError(e); err != nil {
			return nil, err
		}
		streamType, profile, perr := frameStream(f)
		if perr != nil {
			C.rs2_release_frame(f)
			return nil, perr
		}
		switch streamType {
		case C.RS2_STREAM_COLOR:
			if err := c.fillColour(frame, f, profile); err != nil {
				C.rs2_release_frame(f)
				return nil, err
			}
			sawColour = true
		case C.RS2_STREAM_DEPTH:
			if err := c.fillDepth(frame, f); err != nil {
				C.rs2_release_frame(f)
				return nil, err
			}
			sawDepth = true
		}
		C.rs2_release_frame(f)
	}
	if !sawColour {
		return nil, errors.New("aligned frame set carried no colour plane")
	}
	if !sawDepth {
		// Not an error: the agent refuses this frame to anyone who required
		// depth and delivers it to anyone who did not. That distinction is the
		// service's to make, not this helper's.
		frame.Depth = nil
	}
	runtime.KeepAlive(c)
	return frame, nil
}

func frameStream(f *C.rs2_frame) (C.rs2_stream, *C.rs2_stream_profile, error) {
	var e *C.rs2_error
	profile := C.rs2_get_frame_stream_profile(f, &e)
	if err := rsError(e); err != nil {
		return 0, nil, err
	}
	var (
		stream   C.rs2_stream
		format   C.rs2_format
		index    C.int
		uniqueID C.int
		fps      C.int
	)
	C.rs2_get_stream_profile_data(profile, &stream, &format, &index, &uniqueID, &fps, &e)
	if err := rsError(e); err != nil {
		return 0, nil, err
	}
	return stream, (*C.rs2_stream_profile)(unsafe.Pointer(profile)), nil
}

func (c *realSenseCapture) fillColour(frame *agentpbv2.CalibratedFrame, f *C.rs2_frame, profile *C.rs2_stream_profile) error {
	var e *C.rs2_error
	width := int(C.rs2_get_frame_width(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	height := int(C.rs2_get_frame_height(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	stride := int(C.rs2_get_frame_stride_in_bytes(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	size := int(C.rs2_get_frame_data_size(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	data := C.rs2_get_frame_data(f, &e)
	if err := rsError(e); err != nil {
		return err
	}
	frame.Colour = C.GoBytes(data, C.int(size))
	frame.ColourFormat = &agentpb.RawFormat{
		Width:        uint32(width),
		Height:       uint32(height),
		Fourcc:       colourFourcc,
		BytesPerLine: uint32(stride),
	}

	number := uint64(C.rs2_get_frame_number(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	frame.FrameId = number
	frame.CapturedAtNs = captureInstant(f)

	frame.Intrinsics = colourIntrinsics(profile, uint32(width), uint32(height))
	frame.Settings = appliedSettings(f)
	return nil
}

func (c *realSenseCapture) fillDepth(frame *agentpbv2.CalibratedFrame, f *C.rs2_frame) error {
	var e *C.rs2_error
	width := int(C.rs2_get_frame_width(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	height := int(C.rs2_get_frame_height(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	stride := int(C.rs2_get_frame_stride_in_bytes(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	size := int(C.rs2_get_frame_data_size(f, &e))
	if err := rsError(e); err != nil {
		return err
	}
	data := C.rs2_get_frame_data(f, &e)
	if err := rsError(e); err != nil {
		return err
	}
	frame.Depth = &agentpbv2.DepthPlane{
		Data:         C.GoBytes(data, C.int(size)),
		Width:        uint32(width),
		Height:       uint32(height),
		BytesPerLine: uint32(stride),
		ScaleM:       c.depthScal,
		// This frame came out of rs2_align(RS2_STREAM_COLOR), so it IS the
		// colour plane's geometry. The agent asserts that again before fan-out
		// and drops a plane that is not (framesource.Sanitise).
		Alignment: agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR,
	}
	return nil
}

// captureInstant prefers the sensor's own timestamp when librealsense reports
// it in a domain comparable with the host clock, and falls back to arrival time
// otherwise. A hardware-clock timestamp is monotonic in its own epoch and would
// be meaningless to a consumer correlating a frame with anything else.
func captureInstant(f *C.rs2_frame) uint64 {
	var e *C.rs2_error
	domain := C.rs2_get_frame_timestamp_domain(f, &e)
	if err := rsError(e); err != nil {
		return uint64(time.Now().UnixNano())
	}
	if domain != C.RS2_TIMESTAMP_DOMAIN_SYSTEM_TIME && domain != C.RS2_TIMESTAMP_DOMAIN_GLOBAL_TIME {
		return uint64(time.Now().UnixNano())
	}
	ms := float64(C.rs2_get_frame_timestamp(f, &e))
	if err := rsError(e); err != nil || ms <= 0 {
		return uint64(time.Now().UnixNano())
	}
	return uint64(ms * 1e6)
}

// colourIntrinsics reports the colour stream's factory calibration. It is
// MEASURED: a D4xx carries its own calibration, which is exactly what makes a
// physical-size check from pixels worth running.
func colourIntrinsics(profile *C.rs2_stream_profile, width, height uint32) *agentpbv2.CameraIntrinsics {
	var (
		e     *C.rs2_error
		intr  C.rs2_intrinsics
		model = "measured: RealSense colour stream profile"
	)
	C.rs2_get_video_stream_intrinsics(profile, &intr, &e)
	if err := rsError(e); err != nil {
		return nil
	}
	return &agentpbv2.CameraIntrinsics{
		Fx:         float32(intr.fx),
		Fy:         float32(intr.fy),
		Cx:         float32(intr.ppx),
		Cy:         float32(intr.ppy),
		Width:      width,
		Height:     height,
		Provenance: agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED,
		Note:       model,
	}
}

// appliedSettings reads back what the colour sensor actually used.
//
// exposure_unit_us is deliberately left at 0. The RealSense colour sensor's
// RS2_OPTION_EXPOSURE is a UVC control whose unit differs between firmware
// revisions, and a converted microsecond figure derived from a guessed unit is
// worse than an honest raw number: a consumer can see that the unit is unknown
// and refuse, where it cannot see that a converted value is wrong.
func appliedSettings(f *C.rs2_frame) *agentpbv2.CaptureSettings {
	var e *C.rs2_error
	sensor := C.rs2_get_frame_sensor(f, &e)
	if err := rsError(e); err != nil || sensor == nil {
		return nil
	}
	defer C.rs2_delete_sensor(sensor)
	opts := (*C.rs2_options)(unsafe.Pointer(sensor))

	readOption := func(opt C.rs2_option) (float32, bool) {
		var oe *C.rs2_error
		if C.rs2_supports_option(opts, opt, &oe) == 0 {
			_ = rsError(oe)
			return 0, false
		}
		if err := rsError(oe); err != nil {
			return 0, false
		}
		v := float32(C.rs2_get_option(opts, opt, &oe))
		if err := rsError(oe); err != nil {
			return 0, false
		}
		return v, true
	}

	settings := &agentpbv2.CaptureSettings{}
	exposure, haveExposure := readOption(C.RS2_OPTION_EXPOSURE)
	gain, haveGain := readOption(C.RS2_OPTION_GAIN)
	auto, haveAuto := readOption(C.RS2_OPTION_ENABLE_AUTO_EXPOSURE)
	if !haveExposure && !haveGain && !haveAuto {
		// Nothing readable: report nothing rather than a zero-valued message
		// that would satisfy a CAPTURE_SETTINGS requirement while saying
		// nothing at all.
		return nil
	}
	settings.ExposureRaw = uint32(exposure)
	settings.Gain = gain
	settings.AutoExposure = auto != 0
	return settings
}
