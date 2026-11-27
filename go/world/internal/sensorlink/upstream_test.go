package sensorlink

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// This opt-in test starts the actual upstream executable built by the caller.
// It does not replace that process with the World simulator or a fake peer.
func TestUpstreamSimulatorInterop(t *testing.T) {
	binary := os.Getenv("WENDY_SENSORLINK_UPSTREAM_SIM")
	if binary == "" {
		t.Skip("set WENDY_SENSORLINK_UPSTREAM_SIM to an upstream sensorlink-sim executable")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("upstream executable must be an absolute path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.SetRGBA(x, y, color.RGBA{R: byte(x), G: byte(y), B: 77, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "camera.jpg")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	cmd := exec.CommandContext(ctx, binary, "--addr", addr, "--jpeg", path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	var conn net.Conn
	for ctx.Err() == nil {
		conn, err = (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "tcp", addr)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := Connect(ctx, conn, []uint32{1}, Options{HandshakeTimeout: time.Second, IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v := s.Manifest.Sensors[0].GetVideo()
	if v == nil || v.Width != 640 || v.Height != 480 || v.Codec != pb.VideoFormat_MJPEG {
		t.Fatal(s.Manifest)
	}
	var previous uint64
	for seq := uint32(0); seq < 3; seq++ {
		f, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.ChannelID != 1 || f.Seq != seq || f.TsUs <= previous || !bytes.Equal(f.Payload, encoded.Bytes()) || f.Flags != FlagKeyframe {
			t.Fatalf("upstream frame mismatch: channel=%d seq=%d ts=%d bytes=%d", f.ChannelID, f.Seq, f.TsUs, len(f.Payload))
		}
		previous = f.TsUs
	}
}
