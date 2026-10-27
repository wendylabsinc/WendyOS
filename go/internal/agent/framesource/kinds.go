package framesource

// The capture-technology tags CalibratedSource.kind carries. One vendor, one
// drawer: each kind's device knowledge lives in its own package (RealSense in
// go/internal/agent/realsense), never in shared files.
const (
	KindRealSense = "realsense"
	KindV4L2      = "v4l2"
)

// MaxRecordBytes bounds one calibrated frame however it is carried. A
// 1920x1080 YUYV colour plane (4.0 MiB) beside a 1080p-aligned 16-bit depth
// plane (4.0 MiB) is a little over 8 MiB, so the cap has room for the largest
// frame a D4xx can produce while still bounding what a subscriber must be
// prepared to receive.
const MaxRecordBytes = 32 << 20
