package ros2camera

// CameraWriter is the exported form of the internal cameraWriter, so other
// packages (e.g. mcusource) can pump frames into a loopback node without
// re-implementing the V4L2 write path.
type CameraWriter interface {
	WriteFrame(frame Frame) error
	Close() error
}

// NewFrameWriter returns a CameraWriter that writes MJPEG/H264 frames to the
// given /dev/videoN path. On non-Linux builds the underlying writer is a stub.
func NewFrameWriter(path string) CameraWriter { return newFrameWriter(path) }
