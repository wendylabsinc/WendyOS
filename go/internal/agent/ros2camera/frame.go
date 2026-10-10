// Package ros2camera discovers ROS 2 image writers and exposes their frames as
// ordinary V4L2 cameras through Wendy's v4l2loopback support.
package ros2camera

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

const (
	TypeImage           = "sensor_msgs::msg::dds_::Image_"
	TypeCompressedImage = "sensor_msgs::msg::dds_::CompressedImage_"
	TypeGo2FrontVideo   = "unitree_go::msg::dds_::Go2FrontVideoData_"
	maxFrameDimension   = 4096
	maxFramePixels      = 4096 * 2160
	maxRawFramePixels   = 1920 * 1080
)

var ErrUnsupportedEncoding = errors.New("unsupported ROS 2 image encoding")

// Codec is the frame format written to the V4L2 loopback device. MJPEG and
// H.264 carry compressed frames; BGR24 carries raw packed pixels, for feeds
// whose producer hands over uncompressed planes (the RealSense bridge node).
type Codec uint8

const (
	CodecMJPEG Codec = iota + 1
	CodecH264
	CodecBGR24
)

// Frame is one encoded camera frame and the format the loopback writer must
// advertise. Keeping the codec with the bytes avoids transcoding the Go2's
// native H.264 stream merely to fit the JPEG-oriented ROS message paths.
type Frame struct {
	Data          []byte
	Width, Height int
	Codec         Codec
}

// SupportsType reports whether typeName carries a video frame this package can
// decode. DDS and ros2cli spellings are both accepted.
func SupportsType(typeName string) bool {
	switch typeName {
	case TypeImage, "sensor_msgs/msg/Image",
		TypeCompressedImage, "sensor_msgs/msg/CompressedImage",
		TypeGo2FrontVideo, "unitree_go/msg/Go2FrontVideoData":
		return true
	default:
		return false
	}
}

// TopicName turns ROS 2's DDS wire spelling (rt/foo) into the spelling users
// pass to ros2cli (/foo).
func TopicName(topic string) string {
	topic = strings.TrimPrefix(topic, "rt/")
	if !strings.HasPrefix(topic, "/") {
		topic = "/" + topic
	}
	return topic
}

// DecodeFrame extracts one frame from a serialized ROS 2 sample. Standard ROS
// image messages become MJPEG. Go2 firmware is seen in two wire layouts: the
// SDK-declared three-JPEG form and a live single-resolution H.264 form.
func DecodeFrame(typeName string, payload []byte) (Frame, error) {
	if typeName == TypeGo2FrontVideo || typeName == "unitree_go/msg/Go2FrontVideoData" {
		return decodeGo2Frame(payload)
	}
	data, width, height, err := DecodeJPEG(typeName, payload)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Data: data, Width: width, Height: height, Codec: CodecMJPEG}, nil
}

// DecodeJPEG extracts a frame from a serialized ROS 2 sample and returns a
// complete JPEG plus its dimensions. CompressedImage and the Go2's native
// front-video message stay compressed; raw Image frames are converted with the
// standard library so the v4l2loopback writer has one stable MJPEG format.
func DecodeJPEG(typeName string, payload []byte) ([]byte, int, int, error) {
	switch typeName {
	case TypeCompressedImage, "sensor_msgs/msg/CompressedImage":
		return decodeCompressedImage(payload)
	case TypeImage, "sensor_msgs/msg/Image":
		return decodeRawImage(payload)
	case TypeGo2FrontVideo, "unitree_go/msg/Go2FrontVideoData":
		return decodeGo2FrontVideo(payload)
	default:
		return nil, 0, 0, fmt.Errorf("%w: message type %s", ErrUnsupportedEncoding, typeName)
	}
}

func skipHeader(d *cdr.Decoder) error {
	if _, err := d.Int32(); err != nil {
		return fmt.Errorf("header.stamp.sec: %w", err)
	}
	if _, err := d.Uint32(); err != nil {
		return fmt.Errorf("header.stamp.nanosec: %w", err)
	}
	if err := d.SkipString(); err != nil {
		return fmt.Errorf("header.frame_id: %w", err)
	}
	return nil
}

func jpegDimensions(frame []byte) ([]byte, int, int, error) {
	// CompressedImage permits other formats, but this bridge promises MJPEG and
	// should never decompress attacker-controlled PNG or WebP payloads merely to
	// convert them. JPEG header parsing is bounded and the bytes pass through.
	if len(frame) < 2 || frame[0] != 0xff || frame[1] != 0xd8 {
		return nil, 0, 0, fmt.Errorf("%w: compressed frame is not JPEG", ErrUnsupportedEncoding)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decoding compressed image header: %w", err)
	}
	if format != "jpeg" {
		return nil, 0, 0, fmt.Errorf("%w: compressed frame format %s", ErrUnsupportedEncoding, format)
	}
	if err := validateDimensions(cfg.Width, cfg.Height); err != nil {
		return nil, 0, 0, err
	}
	return frame, cfg.Width, cfg.Height, nil
}

func validateDimensions(width, height int) error {
	if width <= 0 || height <= 0 || width > maxFrameDimension || height > maxFrameDimension || uint64(width)*uint64(height) > maxFramePixels {
		return fmt.Errorf("invalid image dimensions %dx%d", width, height)
	}
	return nil
}

func decodeCompressedImage(payload []byte) ([]byte, int, int, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, 0, 0, err
	}
	if err := skipHeader(d); err != nil {
		return nil, 0, 0, err
	}
	if _, err := d.String(); err != nil { // format (jpeg/png plus optional transport metadata)
		return nil, 0, 0, fmt.Errorf("format: %w", err)
	}
	frame, err := d.Bytes()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("data: %w", err)
	}
	return jpegDimensions(frame)
}

func decodeGo2FrontVideo(payload []byte) ([]byte, int, int, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, 0, 0, err
	}
	if _, err := d.Uint64(); err != nil {
		return nil, 0, 0, fmt.Errorf("time_frame: %w", err)
	}
	video720p, err := d.Bytes()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("video720p: %w", err)
	}
	video360p, err := d.Bytes()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("video360p: %w", err)
	}
	video180p, err := d.Bytes()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("video180p: %w", err)
	}
	// Prefer the native 720p stream, falling back in descending resolution for
	// firmware variants that leave one of the three fields empty.
	for _, frame := range [][]byte{video720p, video360p, video180p} {
		if len(frame) != 0 {
			return jpegDimensions(frame)
		}
	}
	return nil, 0, 0, errors.New("Go2 front-video sample contains no image")
}

func decodeGo2Frame(payload []byte) (Frame, error) {
	h264, matchedH264Layout, h264Err := decodeGo2H264Frame(payload)
	if matchedH264Layout && h264Err == nil {
		return h264, nil
	}
	jpegFrame, width, height, jpegErr := decodeGo2FrontVideo(payload)
	if jpegErr == nil {
		return Frame{Data: jpegFrame, Width: width, Height: height, Codec: CodecMJPEG}, nil
	}
	if matchedH264Layout {
		return Frame{}, h264Err
	}
	return Frame{}, jpegErr
}

func decodeGo2H264Frame(payload []byte) (Frame, bool, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return Frame{}, false, err
	}
	if _, err := d.Uint64(); err != nil {
		return Frame{}, false, fmt.Errorf("time_frame: %w", err)
	}
	resolution, err := d.Uint32()
	if err != nil {
		return Frame{}, false, fmt.Errorf("resolution: %w", err)
	}
	width, height, ok := go2ResolutionDimensions(resolution)
	if !ok {
		return Frame{}, false, nil
	}
	data, err := d.Bytes()
	if err != nil {
		return Frame{}, true, fmt.Errorf("video: %w", err)
	}
	// XCDR may leave at most three bytes of alignment padding at the end of a
	// top-level sample. Anything larger means this is not the observed layout.
	if d.Remaining() > 3 {
		return Frame{}, true, fmt.Errorf("video: %d unexpected trailing bytes", d.Remaining())
	}
	if !hasAnnexBStartCode(data) {
		return Frame{}, true, fmt.Errorf("%w: Go2 frame is not Annex-B H.264", ErrUnsupportedEncoding)
	}
	return Frame{Data: data, Width: width, Height: height, Codec: CodecH264}, true, nil
}

func go2ResolutionDimensions(resolution uint32) (width, height int, ok bool) {
	switch resolution {
	case 180, 360, 720:
		height = int(resolution)
		return height * 16 / 9, height, true
	default:
		return 0, 0, false
	}
}

func hasAnnexBStartCode(frame []byte) bool {
	return len(frame) >= 4 && frame[0] == 0 && frame[1] == 0 &&
		(frame[2] == 1 || (len(frame) >= 5 && frame[2] == 0 && frame[3] == 1))
}

func decodeRawImage(payload []byte) ([]byte, int, int, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, 0, 0, err
	}
	if err := skipHeader(d); err != nil {
		return nil, 0, 0, err
	}
	height, err := d.Uint32()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("height: %w", err)
	}
	width, err := d.Uint32()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("width: %w", err)
	}
	encoding, err := d.String()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("encoding: %w", err)
	}
	bigEndian, err := d.Uint8()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("is_bigendian: %w", err)
	}
	step, err := d.Uint32()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("step: %w", err)
	}
	if err := validateDimensions(int(width), int(height)); err != nil {
		return nil, 0, 0, err
	}
	// SECURITY: Raw frames require a full colorspace conversion and JPEG encode,
	// unlike compressed frames. Bound that CPU and allocation path to 1080p.
	if uint64(width)*uint64(height) > maxRawFramePixels {
		return nil, 0, 0, fmt.Errorf("raw image dimensions %dx%d exceed the 1080p pixel budget", width, height)
	}
	data, err := d.Bytes()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("data: %w", err)
	}
	if uint64(step)*uint64(height) > uint64(len(data)) {
		return nil, 0, 0, errors.New("invalid ROS 2 image dimensions")
	}

	var img image.Image
	switch strings.ToLower(encoding) {
	case "mono8", "8uc1":
		if step < width {
			return nil, 0, 0, errors.New("ROS 2 image step is smaller than one row")
		}
		gray := image.NewGray(image.Rect(0, 0, int(width), int(height)))
		for y := 0; y < int(height); y++ {
			copy(gray.Pix[y*gray.Stride:y*gray.Stride+int(width)], data[y*int(step):y*int(step)+int(width)])
		}
		img = gray
	case "mono16", "16uc1":
		if step < width*2 {
			return nil, 0, 0, errors.New("ROS 2 image step is smaller than one row")
		}
		gray := image.NewGray(image.Rect(0, 0, int(width), int(height)))
		for y := 0; y < int(height); y++ {
			row := data[y*int(step):]
			for x := 0; x < int(width); x++ {
				if bigEndian != 0 {
					gray.SetGray(x, y, color.Gray{Y: row[x*2]})
				} else {
					gray.SetGray(x, y, color.Gray{Y: row[x*2+1]})
				}
			}
		}
		img = gray
	case "rgb8", "bgr8", "rgba8", "bgra8":
		channels := 3
		if strings.Contains(strings.ToLower(encoding), "a") {
			channels = 4
		}
		if step < width*uint32(channels) {
			return nil, 0, 0, errors.New("ROS 2 image step is smaller than one row")
		}
		rgba := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
		bgr := strings.HasPrefix(strings.ToLower(encoding), "bgr")
		for y := 0; y < int(height); y++ {
			row := data[y*int(step):]
			for x := 0; x < int(width); x++ {
				i := x * channels
				r, g, b := row[i], row[i+1], row[i+2]
				if bgr {
					r, b = b, r
				}
				// Camera alpha channels are transport padding in practice. JPEG has
				// no alpha, so keep the color fully opaque rather than premultiplying
				// dark pixels when a publisher leaves alpha at zero.
				rgba.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
			}
		}
		img = rgba
	default:
		return nil, 0, 0, fmt.Errorf("%w: %s", ErrUnsupportedEncoding, encoding)
	}
	return encodeJPEG(img)
}

func encodeJPEG(img image.Image) ([]byte, int, int, error) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, 0, 0, fmt.Errorf("encoding JPEG: %w", err)
	}
	b := img.Bounds()
	return out.Bytes(), b.Dx(), b.Dy(), nil
}
