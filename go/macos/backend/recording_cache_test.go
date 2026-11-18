package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

func TestRecordingDiskCacheVerifiedAndScoped(t *testing.T) {
	payload := []byte("recorded clip")
	file := data.File{Size: int64(len(payload)), SHA256: fmt.Sprintf("%x", sha256.Sum256(payload))}
	ctx := context.WithValue(context.WithValue(context.Background(), recordingCacheRoot{}, t.TempDir()), recordingCacheScope{}, "account")
	name := recordingCachePath(ctx, file)
	t.Cleanup(func() { os.Remove(name) })
	var out bytes.Buffer
	err := cacheRecordingDownload(ctx, file, &out, func(w io.Writer) error { _, err := w.Write(payload); return err })
	if err != nil || !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("download: %v %q", err, out.Bytes())
	}
	out.Reset()
	if hit, err := cachedRecordingFile(ctx, file, &out); !hit || err != nil || !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("reuse: %v %v %q", hit, err, out.Bytes())
	}
	other := context.WithValue(context.Background(), recordingCacheScope{}, "another account")
	if recordingCachePath(other, file) == name {
		t.Fatal("account scope reused")
	}
	if err := os.WriteFile(name, []byte("corrupt clip!"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if hit, _ := cachedRecordingFile(ctx, file, &out); hit || out.Len() != 0 {
		t.Fatal("corrupted cache served")
	}
	if err := cacheRecordingDownload(ctx, file, &out, func(w io.Writer) error { w.Write(payload); return io.ErrUnexpectedEOF }); err == nil {
		t.Fatal("download failure hidden")
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatal("failed download persisted")
	}
}

type brokenCacheWriter struct{}

func (brokenCacheWriter) Write([]byte) (int, error) { return 0, io.ErrShortWrite }
func TestOptionalCacheFailureDoesNotBreakPlayback(t *testing.T) {
	var out bytes.Buffer
	mirror := optionalCacheWriter{target: &out, cache: brokenCacheWriter{}}
	if _, err := mirror.Write([]byte("clip")); err != nil || out.String() != "clip" || !mirror.failed {
		t.Fatalf("optional write affected playback: %v", err)
	}
}
