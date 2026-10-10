package services

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// The RealSense bridge re-exposes a calibrated capture's colour plane on a
// v4l2loopback node, so StreamVideo keeps serving the camera while the
// capture helper owns its device nodes
// (specs/2026-10-10-realsense-single-owner-design.md §2-§3).
//
// It is the third user of loopback re-exposure in this agent, after sensor
// pairing (a remote device's cameras pumped into local nodes) and the
// two-plane data path (the producer hub pumped into aux nodes); the frames
// simply come from a calibrated hub this time. resolveSource then serves a
// StreamVideo request for the owned colour node from the bridge node, through
// the ownership table's redirect.

// realSenseBridge builds one bridgedCapture per running RealSense capture.
// Every dependency is a seam: the loopback manager is the video service's own
// (nil-safe, like everywhere it is used), and the writer and colour-node
// probe are injectable so the bridge is testable without a kernel module.
type realSenseBridge struct {
	logger *zap.Logger
	loop   cameraLoopback
	// writerFor opens the pump's writer onto the bridge node.
	writerFor func(path string) ros2camera.CameraWriter
	// colourNodeFor picks the colour node out of a capture's claimed nodes --
	// the one StreamVideo callers actually name, and so the one to redirect.
	colourNodeFor func(nodes []string) string
	// vacate asks the video service to end its own producers on the given
	// nodes because ownership is changing (the handoff, design §4): before the
	// helper opens the claimed device, and again when the bridge node is about
	// to vanish. Nil-safe: without it, a held node fails the helper's open
	// exactly as before.
	vacate func(nodes []string, msg string)
}

func newRealSenseBridge(logger *zap.Logger, loop cameraLoopback) *realSenseBridge {
	return &realSenseBridge{
		logger:        logger,
		loop:          loop,
		writerFor:     ros2camera.NewFrameWriter,
		colourNodeFor: realSenseColourNode,
	}
}

// realSenseColourNode picks the node that advertises a colour capture format.
// Read-only format enumeration is safe while librealsense streams from the
// module (same contract DetectRealSense relies on). Empty when no claimed node
// says it carries colour, in which case there is nothing to redirect.
var realSenseColourNode = func(nodes []string) string {
	for _, n := range nodes {
		if len(enumerateRawFrameSizes(n, v4l2PixFmtYUYV)) > 0 ||
			len(enumerateRawFrameSizes(n, v4l2PixFmtBGR24)) > 0 {
			return n
		}
	}
	return ""
}

// bridgedCapture is one capture's live bridge: its node, its pump, and the
// redirect it stands behind.
type bridgedCapture struct {
	bridge *realSenseBridge
	source string
	nodeNr int
	writer ros2camera.CameraWriter

	mu     sync.Mutex
	latest *agentpbv2.CalibratedFrame
	bell   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	warned bool
}

// start brings the bridge up for one capture and wires its tap into hub. Nil
// when the bridge cannot exist (no loopback manager, no module, no colour
// node, band exhausted): the capture then runs exactly as it would have
// before this design, lockout and all, and the refusal still names the owner.
func (b *realSenseBridge) start(ctx context.Context, hub *frameHub, source string, claimedNodes []string) *bridgedCapture {
	if b == nil || b.loop == nil {
		return nil
	}
	colour := b.colourNodeFor(claimedNodes)
	if colour == "" {
		b.logger.Warn("realsense bridge: no claimed node advertises colour; StreamVideo stays locked out",
			zap.String("source", source), zap.Strings("nodes", claimedNodes))
		return nil
	}
	nr, err := b.loop.AllocateAuxNodeNumber()
	if err != nil {
		b.logger.Warn("realsense bridge: no loopback node available", zap.String("source", source), zap.Error(err))
		return nil
	}
	if err := b.loop.EnsureAuxNode(ctx, nr, "wendy-realsense-bridge"); err != nil {
		b.logger.Warn("realsense bridge: creating loopback node", zap.String("source", source), zap.Error(err))
		return nil
	}
	nodePath := fmt.Sprintf("/dev/video%d", nr)
	bc := &bridgedCapture{
		bridge: b,
		source: source,
		nodeNr: nr,
		writer: b.writerFor(nodePath),
		bell:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	hub.setTap(bc.offer)
	go bc.pump()
	// The redirect stands BEFORE the video service is asked to vacate, so a
	// subscriber reconnecting from the vacate lands on the bridge node, never
	// back on the device the helper is about to open.
	cameraOwners.setBridge(source, colour, nodePath)
	if b.vacate != nil {
		b.vacate(claimedNodes,
			"video stream ended: a calibrated frame capture took ownership of this camera; reconnect to join the stream served from it")
	}
	b.logger.Info("realsense bridge up: StreamVideo served from the calibrated capture",
		zap.String("source", source), zap.String("colour_node", colour), zap.String("bridge_node", nodePath))
	return bc
}

// offer is the hub's tap: latest-wins into the pump's slot, never blocking
// publish (which runs under the hub's lock).
func (bc *bridgedCapture) offer(frame *agentpbv2.CalibratedFrame) {
	bc.mu.Lock()
	bc.latest = frame
	bc.mu.Unlock()
	select {
	case bc.bell <- struct{}{}:
	default:
	}
}

// pump writes each new colour plane to the bridge node. A frame whose colour
// is not the BGR3 the node advertises is dropped with one warning: writing it
// anyway would hand every reader bytes in a format the node lied about.
func (bc *bridgedCapture) pump() {
	defer close(bc.done)
	for {
		select {
		case <-bc.stop:
			return
		case <-bc.bell:
		}
		bc.mu.Lock()
		frame := bc.latest
		bc.mu.Unlock()
		if frame == nil {
			continue
		}
		cf := frame.GetColourFormat()
		if cf.GetFourcc() != "BGR3" {
			if !bc.warned {
				bc.warned = true
				bc.bridge.logger.Warn("realsense bridge: colour is not BGR3; not writing to the bridge node",
					zap.String("source", bc.source), zap.String("fourcc", cf.GetFourcc()))
			}
			continue
		}
		err := bc.writer.WriteFrame(ros2camera.Frame{
			Data:   frame.GetColour(),
			Width:  int(cf.GetWidth()),
			Height: int(cf.GetHeight()),
			Codec:  ros2camera.CodecBGR24,
		})
		if err != nil && !bc.warned {
			bc.warned = true
			bc.bridge.logger.Warn("realsense bridge: writing to the bridge node",
				zap.String("source", bc.source), zap.Error(err))
		}
	}
}

// teardown clears the redirect, bounces the bridge node's own subscribers,
// ends the pump, and removes the node -- in that order. The redirect goes
// first so a reconnecting subscriber resolves the real device (free again: the
// helper has already exited by the time this deferred teardown runs), and the
// bounce carries the same recoverable restart signal as the takeover, so a
// viewer rides the ownership change in both directions without an error.
func (bc *bridgedCapture) teardown() {
	if bc == nil {
		return
	}
	cameraOwners.clearBridge(bc.source)
	nodePath := fmt.Sprintf("/dev/video%d", bc.nodeNr)
	if bc.bridge.vacate != nil {
		bc.bridge.vacate([]string{nodePath},
			"video stream ended: the calibrated frame capture serving this camera ended; reconnect to join the camera directly")
	}
	close(bc.stop)
	<-bc.done
	if err := bc.writer.Close(); err != nil {
		bc.bridge.logger.Warn("realsense bridge: closing the node writer",
			zap.String("source", bc.source), zap.Error(err))
	}
	bc.bridge.loop.RemoveAuxNode(bc.nodeNr)
}
