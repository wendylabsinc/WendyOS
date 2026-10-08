package services

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	sharedJPEGMaxBytes = 8 << 20
	sharedJPEGMaxAge   = 2 * time.Second
	sharedJPEGPeriod   = 100 * time.Millisecond
)

// A shared JPEG feed belongs to the process holding the selected camera. Only
// that process's listening sockets are candidates; no host port scan, redirects,
// proxy environment, or camera-app-specific port is involved.
type sharedCameraEndpoint struct {
	url                    string
	cameraFD, socketFD     string
	cameraLink, socketLink string
}

func (e sharedCameraEndpoint) stillOwned() bool {
	camera, err := os.Readlink(e.cameraFD)
	if err != nil || camera != e.cameraLink {
		return false
	}
	socket, err := os.Readlink(e.socketFD)
	return err == nil && socket == e.socketLink
}

// A process owning several color cameras cannot associate an unqualified
// /frame.jpg with one of them. Depth/IR and metadata nodes do not count as color
// cameras. Refuse ambiguity instead of returning another camera's picture.
func sharedCameraEndpoints(ctx context.Context, proc, path string, isColor func(string) bool) []sharedCameraEndpoint {
	if !isColor(path) {
		return nil
	}
	selfNS, err := os.Stat(filepath.Join(proc, "self/ns/net"))
	if err != nil {
		return nil
	}
	pids, _ := os.ReadDir(proc)
	var out []sharedCameraEndpoint
	for _, pid := range pids {
		if ctx.Err() != nil || len(out) >= 8 {
			break
		}
		if n, err := strconv.Atoi(pid.Name()); err != nil || n == os.Getpid() {
			continue
		}
		base := filepath.Join(proc, pid.Name())
		ns, err := os.Stat(filepath.Join(base, "ns/net"))
		if err != nil || !os.SameFile(selfNS, ns) {
			continue // a private container loopback is not the host loopback
		}
		fds, _ := os.ReadDir(filepath.Join(base, "fd"))
		cameras, sockets := map[string]string{}, map[string]string{}
		for _, fd := range fds {
			file := filepath.Join(base, "fd", fd.Name())
			link, err := os.Readlink(file)
			if err != nil {
				continue
			}
			if strings.HasPrefix(link, "/dev/video") {
				if n, err := strconv.Atoi(strings.TrimPrefix(link, "/dev/video")); err == nil && n >= 0 && n <= maxVideoDeviceID {
					cameras[link] = file
				}
			} else if strings.HasPrefix(link, "socket:[") && strings.HasSuffix(link, "]") {
				sockets[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = file
			}
		}
		cameraFD := cameras[path]
		if cameraFD == "" {
			continue
		}
		ambiguous := false
		for camera := range cameras {
			if camera != path && isColor(camera) {
				ambiguous = true
				break
			}
		}
		if ambiguous {
			continue
		}
		for _, table := range []string{"tcp", "tcp6"} {
			f, err := os.Open(filepath.Join(base, "net", table))
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(io.LimitReader(f, 1<<20))
			for scanner.Scan() && len(out) < 8 {
				fields := strings.Fields(scanner.Text())
				if len(fields) < 10 || fields[3] != "0A" || sockets[fields[9]] == "" {
					continue
				}
				addr, portHex, ok := strings.Cut(fields[1], ":")
				if !ok {
					continue
				}
				host := "127.0.0.1"
				switch addr {
				case "00000000", "0100007F":
				case "00000000000000000000000000000000", "00000000000000000000000001000000":
					host = "::1"
				default:
					continue
				}
				port, err := strconv.ParseUint(portHex, 16, 16)
				if err != nil || port == 0 {
					continue
				}
				out = append(out, sharedCameraEndpoint{
					url:      "http://" + net.JoinHostPort(host, strconv.Itoa(int(port))) + "/frame.jpg",
					cameraFD: cameraFD, cameraLink: path,
					socketFD: sockets[fields[9]], socketLink: "socket:[" + fields[9] + "]",
				})
			}
			_ = f.Close()
		}
	}
	return out
}

func sharedColorCamera(path string) bool {
	for _, format := range []uint32{v4l2PixFmtYUYV, v4l2PixFmtUYVY, v4l2PixFmtMJPEG} {
		if len(enumerateRawFrameSizes(path, format)) != 0 {
			return true
		}
	}
	return false
}

type sharedCameraJPEG struct {
	data          []byte
	width, height int
	captured      int64
}

func readSharedCameraJPEG(ctx context.Context, client *http.Client, endpoint sharedCameraEndpoint) (sharedCameraJPEG, error) {
	var frame sharedCameraJPEG
	if !endpoint.stillOwned() {
		return frame, errors.New("camera feed owner changed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.url, nil)
	if err != nil {
		return frame, err
	}
	req.Header.Set("Accept", "image/jpeg")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return frame, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return frame, fmt.Errorf("shared camera returned HTTP %d", resp.StatusCode)
	}
	if strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]) != "image/jpeg" {
		return frame, errors.New("shared camera did not return image/jpeg")
	}
	frame.captured, err = strconv.ParseInt(resp.Header.Get("X-Captured-At-Unix-Ns"), 10, 64)
	if err != nil || frame.captured <= 0 {
		return frame, errors.New("shared camera did not report its capture timestamp")
	}
	frame.data, err = io.ReadAll(io.LimitReader(resp.Body, sharedJPEGMaxBytes+1))
	if err != nil {
		return frame, err
	}
	if len(frame.data) > sharedJPEGMaxBytes {
		return frame, errors.New("shared camera JPEG exceeds 8 MiB")
	}
	age := time.Since(time.Unix(0, frame.captured))
	if age < -sharedJPEGMaxAge || age > sharedJPEGMaxAge {
		return frame, errors.New("shared camera returned a stale capture timestamp")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame.data))
	if err != nil || cfg.Width < minFrameDimension || cfg.Height < minFrameDimension || cfg.Width > 4096 || cfg.Height > 2160 {
		return frame, errors.New("shared camera returned an invalid or oversized JPEG")
	}
	// DecodeConfig alone accepts truncated/corrupt bodies after the SOF marker.
	if _, err := jpeg.Decode(bytes.NewReader(frame.data)); err != nil {
		return frame, fmt.Errorf("invalid shared camera JPEG: %w", err)
	}
	if !endpoint.stillOwned() {
		return frame, errors.New("camera feed owner changed during capture")
	}
	frame.width, frame.height = cfg.Width, cfg.Height
	return frame, nil
}

func sharedCameraHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Try only before the hub has delivered frames. Once selected, this feed uses
// the same producer hub as native capture, including cancellation and fan-out.
func (s *VideoService) streamSharedCamera(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest, sink rawSink) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := sharedCameraHTTPClient()
	defer client.CloseIdleConnections()
	for _, endpoint := range sharedCameraEndpoints(probeCtx, "/proc", path, sharedColorCamera) {
		first, err := readSharedCameraJPEG(probeCtx, client, endpoint)
		if err != nil {
			s.logger.Debug("camera owner has no usable shared JPEG feed", zap.String("device", path), zap.Error(err))
			continue
		}
		if req.GetCodec() == agentpb.VideoCodec_VIDEO_CODEC_RAW {
			return true, errRawUnavailable("camera owner publishes JPEG; original raw frames are not available")
		}
		s.logger.Info("capturing shared JPEG feed from camera owner", zap.String("device", path))
		return true, s.streamSharedJPEG(ctx, broadcast, path, req, sink, client, endpoint, first)
	}
	return false, nil
}

func (s *VideoService) streamSharedJPEG(ctx context.Context, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool, path string, req *agentpb.StreamVideoRequest, sink rawSink, client *http.Client, endpoint sharedCameraEndpoint, first sharedCameraJPEG) error {
	gst, err := resolveGSTBinary("gst-launch-1.0")
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	inspect, err := resolveGSTBinary("gst-inspect-1.0")
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	enc, err := findGStreamerEncoder(inspect)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	plan, err := planSharedJPEGPipeline(gst, path, req, enc, first)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	feedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r, w := io.Pipe()
	done := make(chan struct{})
	var feedErr error
	go func() {
		defer close(done)
		feedErr = pumpSharedJPEG(feedCtx, w, client, endpoint, first)
		_ = w.CloseWithError(feedErr)
	}()
	defer func() {
		cancel()
		_ = r.Close()
		<-done
	}()
	err = s.runCameraPipeline(ctx, broadcast, path, enc, plan, nil, sink, r)
	cancel()
	_ = r.Close()
	<-done
	if ctx.Err() == nil && feedErr != nil && !errors.Is(feedErr, context.Canceled) && !errors.Is(feedErr, io.ErrClosedPipe) {
		return status.Errorf(codes.Unavailable, "camera owner's shared feed stopped: %v", feedErr)
	}
	return err
}

func pumpSharedJPEG(ctx context.Context, out io.Writer, client *http.Client, endpoint sharedCameraEndpoint, first sharedCameraJPEG) error {
	frame := first
	ticker := time.NewTicker(sharedJPEGPeriod)
	defer ticker.Stop()
	for {
		if time.Since(time.Unix(0, frame.captured)) > sharedJPEGMaxAge {
			return errors.New("shared camera frame expired before encoding")
		}
		if _, err := out.Write(frame.data); err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			next, err := readSharedCameraJPEG(ctx, client, endpoint)
			if err != nil {
				return err
			}
			if next.width != first.width || next.height != first.height {
				return errors.New("shared camera changed image dimensions")
			}
			if next.captured < frame.captured {
				return errors.New("shared camera capture timestamp went backwards")
			}
			if next.captured == frame.captured {
				continue // never encode an old frame again as a new capture
			}
			frame = next
			break
		}
	}
}

func planSharedJPEGPipeline(gst, path string, req *agentpb.StreamVideoRequest, enc gstEncoderResult, first sharedCameraJPEG) (gstPipelinePlan, error) {
	if err := validateStreamParams(path, req); err != nil {
		return gstPipelinePlan{}, err
	}
	if req.GetFramerate() != 0 {
		return gstPipelinePlan{}, errors.New("app-owned JPEG capture uses the source's frame timing; omit the requested frame rate")
	}
	if !isValidGSTElementName(enc.element) {
		return gstPipelinePlan{}, errors.New("invalid shared camera encoder")
	}
	// jpegparse takes the frame rate from its sink caps. Constraining only
	// its output conflicts with its default 0/1 rate and fails negotiation.
	stages := []string{"fdsrc fd=0 do-timestamp=true",
		fmt.Sprintf("image/jpeg,width=%d,height=%d,framerate=10/1", first.width, first.height),
		"jpegparse", "jpegdec", leakyRawQueue}
	if req.GetWidth() != 0 && req.GetHeight() != 0 {
		stages = append(stages, "videoscale", fmt.Sprintf("video/x-raw,width=%d,height=%d", req.GetWidth(), req.GetHeight()))
	}
	stages = append(stages, encoderSegment(enc.element, enc.hasH264Parse, keyframeIntervalFrames(10)), "fdsink fd=1")
	return gstPipelinePlan{
		args:   append([]string{gst, "-q"}, strings.Fields(strings.Join(stages, " ! "))...),
		rawWhy: "camera owner publishes JPEG; original raw frames are not available",
	}, nil
}
