package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// useTestDownloadPolicy scales the idle timeout and retry backoff down so the
// slow-link tests below run in well under a second each.
func useTestDownloadPolicy(t *testing.T, idle time.Duration) {
	t.Helper()
	previous := imageDownloadPolicy
	t.Cleanup(func() { imageDownloadPolicy = previous })
	imageDownloadPolicy = downloadPolicy{
		idleTimeout:   idle,
		maxRetries:    3,
		retryDelay:    time.Millisecond,
		maxRetryDelay: 5 * time.Millisecond,
	}
}

// flakyImageServer serves payload the way a CDN behaves on a bad link: paced,
// refusing HEAD, ignoring Range, or cutting or stalling a body at failAt.
type flakyImageServer struct {
	payload    []byte
	ranges     bool          // honor Range and advertise Accept-Ranges
	lieRanges  bool          // advertise Accept-Ranges on HEAD but ignore Range
	headStatus int           // answer HEAD with this status instead of 200
	piece      int64         // bytes per write; 0 sends the body in one write
	delay      time.Duration // pause after each write
	failAt     int64         // offset at which a body is cut (or stalled); 0 = never
	stall      bool          // hang at failAt instead of dropping the connection
	failEvery  bool          // fail every body that reaches failAt, not only the first

	failed atomic.Bool
	mu     sync.Mutex
	seen   []string // Range header of every GET, "" when absent
}

func (s *flakyImageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead && s.headStatus != 0 {
		w.WriteHeader(s.headStatus)
		return
	}
	size := int64(len(s.payload))
	start, end, status := int64(0), size-1, http.StatusOK
	if s.ranges || (s.lieRanges && r.Method == http.MethodHead) {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if r.Method == http.MethodGet {
		rh := r.Header.Get("Range")
		s.mu.Lock()
		s.seen = append(s.seen, rh)
		s.mu.Unlock()
		if s.ranges && rh != "" {
			if _, err := fmt.Sscanf(rh, "bytes=%d-%d", &start, &end); err != nil || end >= size {
				end = size - 1
			}
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		}
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	piece := s.piece
	if piece <= 0 {
		piece = size
	}
	for off := start; off <= end; {
		// Only a body that already sent bytes is cut once, so the client
		// sees a mid-body failure rather than one its transport retries.
		if s.failAt > 0 && off >= s.failAt && (s.failEvery || (start < s.failAt && s.failed.CompareAndSwap(false, true))) {
			if s.stall {
				<-r.Context().Done()
				return
			}
			panic(http.ErrAbortHandler) // closes the connection mid-body
		}
		n := min(piece, end+1-off)
		if s.failAt > off {
			n = min(n, s.failAt-off)
		}
		if _, err := w.Write(s.payload[off : off+n]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		off += n
		time.Sleep(s.delay)
	}
}

func (s *flakyImageServer) sawRange(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.seen, want)
}

func testImagePayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + i/251) // non-repeating within a range, so a misplaced resume shows
	}
	return b
}

// downloadFromFlakyServer runs the real image download against s and checks
// the file the way the flashpack and install-job callers do: by SHA-256.
func downloadFromFlakyServer(t *testing.T, s *flakyImageServer) (string, error) {
	t.Helper()
	isolateVMImageCache(t)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var lastDownloaded, lastTotal int64
	path, err := downloadImageIntoContext(context.Background(), &imageInfo{DownloadURL: srv.URL + "/image.img.zst"},
		func(downloaded, total int64) {
			mu.Lock()
			lastDownloaded, lastTotal = max(lastDownloaded, downloaded), total
			mu.Unlock()
		})
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { os.Remove(path) })
	sum := sha256.Sum256(s.payload)
	if err := verifySHA256(path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("downloaded image fails verification: %v", err)
	}
	size := int64(len(s.payload))
	if lastDownloaded != size || lastTotal != size {
		t.Fatalf("last progress = %d/%d, want %d/%d", lastDownloaded, lastTotal, size, size)
	}
	return path, nil
}

// A slow link that keeps delivering bytes must never time out, however long
// the whole transfer takes. Each transfer below lasts several idle timeouts.
func TestDownloadImageSlowLinkDoesNotTimeOut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges bool
		size   int
	}{
		{name: "single stream", size: 2 << 10},
		{name: "parallel ranges", ranges: true, size: 12 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTestDownloadPolicy(t, 200*time.Millisecond)
			// 128-byte writes every 25ms: never idle for long, but each stream
			// (16 writes, or 12 per parallel range) outlasts the idle timeout.
			s := &flakyImageServer{payload: testImagePayload(tc.size), ranges: tc.ranges, piece: 128, delay: 25 * time.Millisecond}
			if _, err := downloadFromFlakyServer(t, s); err != nil {
				t.Fatalf("slow but live download failed: %v", err)
			}
		})
	}
}

// A dropped or stalled connection resumes from the last byte written rather
// than starting over, and the resumed file still verifies.
func TestDownloadImageResumesAfterDrop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		server    *flakyImageServer
		wantRange string // Range header some request must carry
	}{
		{
			// A bare 200 HEAD (no Accept-Ranges) means one stream; its GET
			// still resumes, because the resumed request is the range probe.
			name:      "single stream drop",
			server:    &flakyImageServer{ranges: true, headStatus: http.StatusOK, failAt: 5000},
			wantRange: "bytes=5000-",
		},
		{
			name:      "single stream stall",
			server:    &flakyImageServer{ranges: true, headStatus: http.StatusOK, failAt: 5000, stall: true},
			wantRange: "bytes=5000-",
		},
		{
			name:      "parallel range drop",
			server:    &flakyImageServer{ranges: true, failAt: 3000},
			wantRange: "bytes=3000-3071",
		},
		{
			name:      "parallel range stall",
			server:    &flakyImageServer{ranges: true, failAt: 3000, stall: true},
			wantRange: "bytes=3000-3071",
		},
		{
			// A server without range support restarts the body from zero.
			name:      "no range support",
			server:    &flakyImageServer{failAt: 5000},
			wantRange: "bytes=5000-",
		},
		{
			// HEAD claims ranges but GET ignores them: the first 200 stops
			// the parallel ranges and one plain GET fetches the file.
			name:      "ranges advertised but ignored",
			server:    &flakyImageServer{lieRanges: true},
			wantRange: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTestDownloadPolicy(t, 200*time.Millisecond)
			s := tc.server
			s.payload = testImagePayload(8 << 10)
			if _, err := downloadFromFlakyServer(t, s); err != nil {
				t.Fatalf("download did not survive the failure: %v", err)
			}
			if !s.sawRange(tc.wantRange) {
				t.Fatalf("no request with Range %q; saw %q", tc.wantRange, s.seen)
			}
		})
	}
}

// A server that stops sending is abandoned after the idle timeout and a
// bounded number of retries, and leaves no partial file behind.
func TestDownloadImageStallTimesOut(t *testing.T) {
	useTestDownloadPolicy(t, 100*time.Millisecond)
	s := &flakyImageServer{payload: testImagePayload(8 << 10), ranges: true, headStatus: http.StatusMethodNotAllowed, failAt: 4096, stall: true, failEvery: true}
	started := time.Now()
	_, err := downloadFromFlakyServer(t, s)
	if !errors.Is(err, errDownloadStalled) {
		t.Fatalf("err = %v, want a stall error", err)
	}
	// One stalled attempt plus maxRetries stalled resumes, each idleTimeout.
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("gave up after %s; the retry budget is not bounded", elapsed)
	}
	if !s.sawRange("bytes=4096-") {
		t.Fatalf("stalled download was not resumed; saw %q", s.seen)
	}
	dir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	assertVMImageCacheEmpty(t, dir)
}

// A failed HEAD probe must not cost the download its ability to resume.
func TestDownloadImageHEADFailureStillResumes(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			useTestDownloadPolicy(t, 200*time.Millisecond)
			s := &flakyImageServer{payload: testImagePayload(8 << 10), ranges: true, headStatus: status, failAt: 6000}
			if _, err := downloadFromFlakyServer(t, s); err != nil {
				t.Fatalf("download after failed HEAD: %v", err)
			}
			if !s.sawRange("bytes=6000-") {
				t.Fatalf("download restarted instead of resuming; saw %q", s.seen)
			}
		})
	}
}

// Permanent HTTP errors fail at once rather than burning the retry budget.
func TestDownloadImagePermanentErrorIsNotRetried(t *testing.T) {
	useTestDownloadPolicy(t, 200*time.Millisecond)
	isolateVMImageCache(t)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := downloadImageIntoContext(context.Background(), &imageInfo{DownloadURL: srv.URL + "/missing.img"}, nil)
	if err == nil || gets.Load() != 1 {
		t.Fatalf("err = %v after %d GETs, want one failed GET", err, gets.Load())
	}
}
