//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

const (
	flashTestDevice  = "test-board"
	flashTestVersion = "1.2.3"
	flashTestStorage = "nvme"
)

// flashSourceServer publishes a small seekable image the way current manifests
// do, one .img.zst as both path and zst_path, next to its block map. It counts
// requests for the image and downloads of it.
type flashSourceServer struct {
	*httptest.Server
	raw, zst  []byte
	bmap      *Bmap
	imageHits atomic.Int32
	downloads atomic.Int32
}

// How the server answers block map requests.
const (
	bmapServed  = iota
	bmapDown    // fails, as it did in WDY-4088
	bmapGarbage // not a block map
)

// newFlashSourceServer starts the server with an empty OS image cache.
func newFlashSourceServer(t *testing.T, bmapMode int) (*flashSourceServer, *imageInfo) {
	t.Helper()
	isolateVMImageCache(t)
	raw, b := buildSparseImage()
	s := &flashSourceServer{raw: raw, zst: encodeSeekable(t, raw, 4096), bmap: b}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image.img.zst":
			s.imageHits.Add(1)
			http.ServeContent(w, r, "image.img.zst", time.Time{}, bytes.NewReader(s.zst))
		case "/image.img.bmap":
			switch bmapMode {
			case bmapDown:
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			case bmapGarbage:
				io.WriteString(w, "<html>")
			default:
				io.WriteString(w, bmapXML(b))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)

	// downloadImage draws a progress TUI, which needs a terminal.
	old := downloadOSImage
	t.Cleanup(func() { downloadOSImage = old })
	downloadOSImage = func(img *imageInfo) (string, error) {
		s.downloads.Add(1)
		return downloadImageInto(img, nil)
	}

	return s, &imageInfo{
		DownloadURL: s.URL + "/image.img.zst",
		ZstURL:      s.URL + "/image.img.zst",
		BmapURL:     s.URL + "/image.img.bmap",
		Version:     flashTestVersion,
		Storage:     flashTestStorage,
	}
}

func flashCachePath(t *testing.T, ext string) string {
	t.Helper()
	p, err := osCachedPath(flashTestDevice, flashTestVersion, flashTestStorage, ext)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFlashCacheFile(t *testing.T, ext string, data []byte) string {
	t.Helper()
	p := flashCachePath(t, ext)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// assertNoSecondImageCopy fails when the image was also cached under the
// names resolveOSImage uses.
func assertNoSecondImageCopy(t *testing.T) {
	t.Helper()
	for _, ext := range []string{".img", ".zip"} {
		if _, err := os.Stat(flashCachePath(t, ext)); err == nil {
			t.Errorf("image cached a second time as %s", flashCachePath(t, ext))
		}
	}
}

// assertFlashSource checks src against the image s serves: a seekable block
// map write from zstPath when wantBmap, else a sequential write of the whole
// image streamed from zstPath.
func assertFlashSource(t *testing.T, s *flashSourceServer, src *flashSource, zstPath string, wantBmap bool) {
	t.Helper()
	if wantBmap {
		if src.seekableZst != zstPath || src.stream != nil {
			t.Fatalf("want a block map write from %s, got seekableZst=%q stream=%v", zstPath, src.seekableZst, src.stream != nil)
		}
		if src.seekableTotal != mappedBytes(s.bmap) || src.imageSize != s.bmap.ImageSize {
			t.Errorf("seekableTotal=%d imageSize=%d, want %d and %d", src.seekableTotal, src.imageSize, mappedBytes(s.bmap), s.bmap.ImageSize)
		}
		return
	}
	if src.seekableZst != "" || src.bmapPath != "" || src.stream == nil {
		t.Fatalf("want a sequential write, got seekableZst=%q bmapPath=%q stream=%v", src.seekableZst, src.bmapPath, src.stream != nil)
	}
	if src.stream.uncompressedSize != int64(len(s.raw)) {
		t.Errorf("stream size %d, want %d", src.stream.uncompressedSize, len(s.raw))
	}
	got, err := io.ReadAll(src.stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, s.raw) {
		t.Error("streamed image differs from the published image")
	}
}

// WDY-4088: with the image cached, a block map that can't be fetched must not
// send the install off to download the image again.
func TestResolveFlashSourceReusesCachedImage(t *testing.T) {
	tests := []struct {
		name       string
		bmapMode   int
		cachedBmap string // "" none, "same" for this image, "other" for another
		noBmap     bool   // --no-bmap
		noBmapURL  bool   // the manifest publishes no block map
		wantBmap   bool
	}{
		{name: "block map fetched", wantBmap: true},
		{name: "block map fetch fails, cached block map", bmapMode: bmapDown, cachedBmap: "same", wantBmap: true},
		{name: "block map fetch fails, none cached", bmapMode: bmapDown},
		{name: "block map fetch fails, cached block map for another image", bmapMode: bmapDown, cachedBmap: "other"},
		{name: "block map unusable", bmapMode: bmapGarbage},
		{name: "no block map published", noBmapURL: true},
		{name: "--no-bmap", noBmap: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, img := newFlashSourceServer(t, tt.bmapMode)
			zstPath := writeFlashCacheFile(t, ".img.zst", s.zst)
			if tt.cachedBmap != "" {
				cached := *s.bmap
				if tt.cachedBmap == "other" {
					cached.ImageSize *= 2
				}
				writeFlashCacheFile(t, ".bmap", []byte(bmapXML(&cached)))
			}
			if tt.noBmapURL {
				img.BmapURL = ""
			}

			src, err := resolveFlashSource(flashTestDevice, flashTestVersion, flashTestStorage, img, tt.noBmap)
			if err != nil {
				t.Fatalf("resolveFlashSource: %v", err)
			}
			if src.stream != nil {
				defer src.stream.Close()
			}
			if n := s.imageHits.Load(); n != 0 {
				t.Errorf("requested the cached image %d times", n)
			}
			assertNoSecondImageCopy(t)
			assertFlashSource(t, s, src, zstPath, tt.wantBmap)
		})
	}
}

func TestResolveFlashSourceDownloadsWhenNothingCached(t *testing.T) {
	for _, tt := range []struct {
		name     string
		bmapMode int
	}{
		{"block map fetched", bmapServed},
		{"block map fetch fails", bmapDown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, img := newFlashSourceServer(t, tt.bmapMode)

			src, err := resolveFlashSource(flashTestDevice, flashTestVersion, flashTestStorage, img, false)
			if err != nil {
				t.Fatalf("resolveFlashSource: %v", err)
			}
			if src.stream != nil {
				defer src.stream.Close()
			}
			if n := s.downloads.Load(); n != 1 {
				t.Errorf("downloaded the image %d times, want once", n)
			}
			zstPath := flashCachePath(t, ".img.zst")
			if got, err := os.ReadFile(zstPath); err != nil || !bytes.Equal(got, s.zst) {
				t.Fatalf("download not cached at %s: %v", zstPath, err)
			}
			assertNoSecondImageCopy(t)
			assertFlashSource(t, s, src, zstPath, tt.bmapMode == bmapServed)
		})
	}
}
