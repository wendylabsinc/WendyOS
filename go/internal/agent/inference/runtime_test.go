package inference

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

func runtimeArchive(t *testing.T, kind byte, name string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	writer := tar.NewWriter(gz)
	contents := []byte("test executable")
	header := &tar.Header{Name: name, Mode: 0700, Typeflag: kind}
	if kind == tar.TypeReg {
		header.Size = int64(len(contents))
	} else {
		header.Linkname = "../../escape"
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if kind == tar.TypeReg {
		if _, err := writer.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestRuntimeDownloadVerifiesChecksumAndExtractsOnlyExecutable(t *testing.T) {
	for _, test := range []struct {
		name                  string
		kind                  byte
		path                  string
		validSHA, wantSuccess bool
	}{
		{"valid", tar.TypeReg, "uv-test/uv", true, true},
		{"checksum mismatch", tar.TypeReg, "uv-test/uv", false, false},
		{"symlink", tar.TypeSymlink, "uv-test/uv", true, false},
		{"path traversal", tar.TypeReg, "../../uv", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := runtimeArchive(t, test.kind, test.path)
			hash := sha256.Sum256(archive)
			digest := hex.EncodeToString(hash[:])
			if !test.validSHA {
				digest = "wrong"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "uv")
			err := installUV(context.Background(), server.URL, digest, "test", destination)
			if (err == nil) != test.wantSuccess {
				t.Fatalf("install result: %v", err)
			}
			b, readErr := os.ReadFile(destination)
			if test.wantSuccess && (readErr != nil || string(b) != "test executable") {
				t.Fatalf("wrong installed bytes: %s, %v", b, readErr)
			}
			if !test.wantSuccess && readErr == nil {
				t.Fatal("invalid executable installed")
			}
		})
	}
}

func TestRuntimeDownloadHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := installUV(ctx, "https://unused.invalid/uv", "unused", "test", filepath.Join(t.TempDir(), "uv"))
	if err == nil {
		t.Fatal("cancelled runtime download succeeded")
	}
}

// worker.py returns an input's sample unread, with the scored picture's size,
// under these keys.
func TestWorkerResultCarriesSampleAndPictureSize(t *testing.T) {
	sample := Sample{ID: 7, UnixNanos: 11, BootNanos: 13}
	line, err := json.Marshal(Input{SourceID: "v4l2:/dev/video0", Generation: 1, Sample: &sample})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Sample json.RawMessage `json:"sample"`
	}
	if err := json.Unmarshal(line, &sent); err != nil {
		t.Fatal(err)
	}
	prediction := `{"type":"prediction","source_id":"v4l2:/dev/video0","generation":1,"detections":[],"width":640,"height":480,"sample":` + string(sent.Sample) + `}`
	var result Result
	if err := json.Unmarshal([]byte(prediction), &result); err != nil {
		t.Fatal(err)
	}
	if result.Width != 640 || result.Height != 480 || result.Sample == nil || *result.Sample != sample {
		t.Fatalf("result = %+v", result)
	}
}

func TestRuntimeTailIsBounded(t *testing.T) {
	b := &tailBuffer{}
	_, _ = b.Write(bytes.Repeat([]byte("a"), 20000))
	_, _ = b.Write([]byte("last error"))
	if len(b.String()) != 8192 || b.String()[8192-10:] != "last error" {
		t.Fatal("runtime diagnostics are unbounded or lost their tail")
	}
}

// Opt-in integration: exercises the exact embedded runtime and worker, without
// an agent, camera, external notification, or user-authored model code.
func TestManagedRuntimeSmoke(t *testing.T) {
	video := os.Getenv("WENDY_INFERENCE_SMOKE_H264")
	if video == "" {
		t.Skip("set WENDY_INFERENCE_SMOKE_H264 to a person-containing H.264 fixture; downloads runtime/model")
	}
	if !Supported() {
		t.Skip("unsupported runtime platform")
	}
	root := os.Getenv("WENDY_INFERENCE_SMOKE_CACHE")
	if root == "" {
		root = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	factory := &ManagedFactory{Root: root}
	session, err := factory.Start(ctx, data.CampaignInference{Model: "facebook/detr-resnet-50", Revision: "1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b", Labels: []string{"person"}, Threshold: .9, Rate: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	fixtures := []struct{ path, source, encoding string }{{video, "v4l2:/dev/video0", "h264"}}
	if webm := os.Getenv("WENDY_INFERENCE_SMOKE_VP8"); webm != "" {
		fixtures = append(fixtures, struct{ path, source, encoding string }{webm, "ipcamera:1000000", "vp8"})
	}
	feedErr := make(chan error, len(fixtures))
	for _, fixture := range fixtures {
		payload, err := os.ReadFile(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) == 0 {
			t.Fatal("empty video fixture")
		}
		go func() {
			for ctx.Err() == nil {
				for pos := 0; pos < len(payload); pos += 4096 {
					end := min(pos+4096, len(payload))
					if err := session.Send(Input{SourceID: fixture.source, Generation: 1, Encoding: fixture.encoding, Payload: payload[pos:end]}); err != nil {
						feedErr <- err
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
		}()
	}
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	detected := map[string]bool{}
	for {
		select {
		case result, ok := <-session.Results():
			if !ok {
				t.Fatal("runtime exited before prediction")
			}
			if result.Type == "prediction" && len(result.Detections) > 0 {
				if !detected[result.SourceID] {
					t.Logf("managed runtime detected %d people on %s", len(result.Detections), result.SourceID)
				}
				detected[result.SourceID] = true
				if len(detected) == len(fixtures) {
					cancel()
					return
				}
			}
		case err := <-feedErr:
			if err != io.EOF {
				t.Fatal(err)
			}
		case <-timer.C:
			t.Fatalf("runtime did not detect people on every feed: %v", detected)
		}
	}
}

func TestRuntimeDownloadRejectsOversizedArchiveEntry(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "unrelated", Mode: 0600, Size: 257 << 20}); err != nil {
		t.Fatal(err)
	}
	// Only the oversized header is needed: reject before reading its body.
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(archive.Bytes())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive.Bytes()) }))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "uv")
	err := installUV(context.Background(), server.URL, hex.EncodeToString(hash[:]), "test", destination)
	if err == nil || !strings.Contains(err.Error(), "decompressed limit") {
		t.Fatalf("expected expansion limit, got %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("unexpected executable: %v", err)
	}
}

func TestRuntimeEnvironmentDoesNotForwardAgentSecrets(t *testing.T) {
	for _, key := range []string{"WENDY_TOKEN", "AWS_SECRET_ACCESS_KEY", "HF_TOKEN", "PYTHONPATH"} {
		t.Setenv(key, "agent-secret")
	}
	root := t.TempDir()
	env := strings.Join(runtimeEnvironment(root), "\n")
	if strings.Contains(env, "agent-secret") {
		t.Fatal("agent environment leaked into model worker")
	}
	if !strings.Contains(env, "HOME="+filepath.Join(root, "home")) {
		t.Fatal("model uses agent home")
	}
}

func TestValidCachedUV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uv")
	good := []byte("verified executable")
	digest := sha256.Sum256(good)
	checksum := hex.EncodeToString(digest[:])
	check := func(want bool) {
		t.Helper()
		valid, err := validCachedUV(path, checksum)
		if err != nil || valid != want {
			t.Fatalf("validCachedUV = %v, %v; want %v", valid, err, want)
		}
	}
	check(false)
	if err := os.WriteFile(path, good, 0700); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err := os.WriteFile(path, []byte("modified executable"), 0700); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := os.WriteFile(path, good, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, good, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	check(false)
}
