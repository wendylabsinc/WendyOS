package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
)

func outputFixture(t *testing.T) cameraframe.Frame {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 10, 8)), nil); err != nil {
		t.Fatal(err)
	}
	f, err := cameraframe.NewFrame(cameraframe.Metadata{Version: 1, SourceID: "sensorlink:test:1", SourceSessionID: "host-1", Transport: "tcp", TimestampBasis: "unknown", Codec: "mjpeg", Width: 10, Height: 8, Receipt: cameraframe.Receipt{BootID: "gateway", BeforeBootNS: 10, ReceivedBootNS: 11, AfterBootNS: 12}}, b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExclusiveOutputExactJPEGAndMetadata(t *testing.T) {
	f := outputFixture(t)
	path := filepath.Join(t.TempDir(), "frame")
	r, err := writeFrame(path, f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(path, "frame.jpg"))
	if err != nil || !bytes.Equal(got, f.JPEG) {
		t.Fatal("original JPEG changed", err)
	}
	m, err := os.ReadFile(filepath.Join(path, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := cameraframe.DecodeMetadata(m)
	if err != nil || decoded.JPEGSHA256 != f.Metadata.JPEGSHA256 {
		t.Fatal("metadata mismatch", err)
	}
	sum := sha256.Sum256(m)
	if r.MetadataSHA256 != hex.EncodeToString(sum[:]) || r.MetadataSize != len(m) || r.Published || r.Status != "complete" {
		t.Fatal("incorrect output receipt")
	}
	if _, err := writeFrame(path, f); err == nil {
		t.Fatal("existing output overwritten")
	}
	for _, name := range []string{"frame.jpg", "metadata.json"} {
		info, err := os.Stat(filepath.Join(path, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("unexpected file mode", err)
		}
	}
}

func TestOutputRefusesInvalidFrameAndExistingSymlink(t *testing.T) {
	f := outputFixture(t)
	root := t.TempDir()
	path := filepath.Join(root, "invalid")
	bad := f.Clone()
	bad.JPEG[20] ^= 1
	if _, err := writeFrame(path, bad); err == nil {
		t.Fatal("invalid frame written")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("invalid frame created output")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFrame(link, f); err == nil {
		t.Fatal("existing symlink accepted")
	}
	for _, p := range []string{"relative", "/", "/tmp/../frame"} {
		if _, err := writeFrame(p, f); err == nil {
			t.Fatal("invalid output path accepted")
		}
	}
}
