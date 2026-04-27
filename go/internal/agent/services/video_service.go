package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// VideoService implements agentpb.WendyVideoServiceServer.
type VideoService struct {
	agentpb.UnimplementedWendyVideoServiceServer
	logger         *zap.Logger
	globDevices    func() ([]string, error)
	readDeviceName func(base string) (string, error)
}

// NewVideoService creates a new VideoService.
func NewVideoService(logger *zap.Logger) *VideoService {
	return &VideoService{
		logger: logger,
		globDevices: func() ([]string, error) {
			return filepath.Glob("/dev/video*")
		},
		readDeviceName: func(base string) (string, error) {
			b, err := os.ReadFile(fmt.Sprintf("/sys/class/video4linux/%s/name", base))
			return strings.TrimSpace(string(b)), err
		},
	}
}

// listV4L2Devices enumerates /dev/video* and reads human-readable names from sysfs.
func (s *VideoService) listV4L2Devices() ([]*agentpb.VideoDevice, error) {
	paths, err := s.globDevices()
	if err != nil {
		return nil, err
	}
	var devices []*agentpb.VideoDevice
	for _, path := range paths {
		base := filepath.Base(path)
		numStr := strings.TrimPrefix(base, "video")
		id, err := strconv.ParseUint(numStr, 10, 32)
		if err != nil {
			continue
		}
		name, err := s.readDeviceName(base)
		if err != nil {
			name = base
		}
		devices = append(devices, &agentpb.VideoDevice{
			Id:   uint32(id),
			Name: name,
			Path: path,
		})
	}
	return devices, nil
}

// ListVideoDevices enumerates V4L2 video capture devices.
func (s *VideoService) ListVideoDevices(ctx context.Context, _ *agentpb.ListVideoDevicesRequest) (*agentpb.ListVideoDevicesResponse, error) {
	devices, err := s.listV4L2Devices()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to enumerate video devices: %v", err)
	}
	return &agentpb.ListVideoDevicesResponse{Devices: devices}, nil
}

// hwUnavailableError signals that the V4L2 hardware encoder is not available.
type hwUnavailableError struct{ msg string }

func (e hwUnavailableError) Error() string { return e.msg }

// buildFFmpegArgs constructs the ffmpeg argument list for V4L2 capture.
// hardware=true attempts the device's built-in H.264 encoder; hardware=false uses libx264.
// Width/height are omitted when both are 0; framerate is omitted when 0.
func buildFFmpegArgs(path string, req *agentpb.StreamVideoRequest, hardware bool) []string {
	var args []string
	if hardware {
		args = []string{"-f", "v4l2", "-input_format", "h264"}
	} else {
		args = []string{"-f", "v4l2"}
	}
	if req.GetWidth() > 0 && req.GetHeight() > 0 {
		args = append(args, "-video_size", fmt.Sprintf("%dx%d", req.GetWidth(), req.GetHeight()))
	}
	if req.GetFramerate() > 0 {
		args = append(args, "-framerate", fmt.Sprintf("%d", req.GetFramerate()))
	}
	args = append(args, "-nostdin", "-loglevel", "error", "-i", path)
	if hardware {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency")
	}
	args = append(args, "-f", "h264", "pipe:1")
	return args
}

// StreamVideo streams H.264 frames from a V4L2 camera.
// Tries the hardware encoder first; falls back to libx264 software encoding.
// Note: the HW fallback only triggers when ffmpeg exits within 2 s with no frames
// sent. Devices that fail after 2 s will not fall back to software encoding.
func (s *VideoService) StreamVideo(req *agentpb.StreamVideoRequest, stream grpc.ServerStreamingServer[agentpb.VideoFrame]) error {
	ctx := stream.Context()
	path := fmt.Sprintf("/dev/video%d", req.GetDeviceId())

	if _, err := os.Stat(path); err != nil {
		return status.Errorf(codes.NotFound, "video device %s not found", path)
	}

	err := s.runFFmpeg(ctx, stream, buildFFmpegArgs(path, req, true), true)
	if _, ok := err.(hwUnavailableError); ok {
		s.logger.Info("hardware H.264 encoder not available, falling back to libx264")
		return s.runFFmpeg(ctx, stream, buildFFmpegArgs(path, req, false), false)
	}
	return err
}

// runFFmpeg executes ffmpeg with the given args and streams VideoFrame chunks.
// When detectHW is true and ffmpeg exits within 2 s with no frames sent,
// returns hwUnavailableError so the caller can retry with software encoding.
func (s *VideoService) runFFmpeg(ctx context.Context, stream grpc.ServerStreamingServer[agentpb.VideoFrame], args []string, detectHW bool) (runErr error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create video pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start ffmpeg: %v", err)
	}

	startedAt := time.Now()
	var framesSent int
	var hwFailed bool

	defer func() {
		cmd.Process.Kill()               //nolint:errcheck
		io.Copy(io.Discard, stdout)      // drain so Wait's internal goroutine can exit
		cmd.Wait()                       //nolint:errcheck
		// stderrBuf is safe to read after Wait (no concurrent writer)
		if hwFailed {
			runErr = hwUnavailableError{msg: fmt.Sprintf("hardware encoder not available (stderr: %s)", strings.TrimSpace(stderrBuf.String()))}
			return
		}
		if runErr == nil {
			if msg := strings.TrimSpace(stderrBuf.String()); msg != "" {
				runErr = status.Errorf(codes.Internal, "ffmpeg: %s", msg)
			}
		}
	}()

	const chunkSize = 16 * 1024
	buf := make([]byte, chunkSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, readErr := stdout.Read(buf)
		if n > 0 {
			framesSent++
			data := make([]byte, n)
			copy(data, buf[:n])
			if sendErr := stream.Send(&agentpb.VideoFrame{
				Data:        data,
				TimestampNs: uint64(time.Now().UnixNano()),
			}); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			if detectHW && framesSent == 0 && time.Since(startedAt) < 2*time.Second {
				hwFailed = true
				return nil // defer sets hwUnavailableError after Wait
			}
			return nil // defer checks stderr and sets error if non-empty
		}
	}
}
