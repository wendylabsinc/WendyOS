package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// downloadTestScale turns real durations into test ones: the 60 s idle timeout
// becomes 60 ms, and a 30 min transfer 1.8 s.
const downloadTestScale = 1000

// downloadSite is one of the downloads, other than the OS image itself, that
// follow the OS image download's policy. fetch runs the site's own function
// against a server at baseURL and returns the bytes it produced; a site that
// writes a file puts it in dir.
type downloadSite struct {
	name     string
	oldLimit time.Duration // the deadline the site used to put on the whole transfer
	redirect bool          // the URL redirects to the file, as a GitHub release asset does
	fetch    func(ctx context.Context, baseURL, dir string, payload []byte) ([]byte, error)
}

var downloadSites = []downloadSite{
	{
		name: "simulator image", oldLimit: 30 * time.Minute,
		fetch: func(ctx context.Context, baseURL, dir string, _ []byte) ([]byte, error) {
			path, err := downloadMCPSimulatorImage(ctx, dir, baseURL+"/simulator.img")
			if err != nil {
				return nil, err
			}
			return os.ReadFile(path)
		},
	},
	{
		name: "agent artifact", oldLimit: 5 * time.Minute,
		fetch: func(ctx context.Context, baseURL, _ string, payload []byte) ([]byte, error) {
			sum := sha256.Sum256(payload)
			m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{"v1": {
				Artifacts: map[string]agentManifestArtifact{"darwin-arm64": {Path: "agent/v1/agent.zip", Checksum: hex.EncodeToString(sum[:])}},
			}}}
			raw, _, err := downloadAgentArtifactFromGCS(ctx, baseURL, m, "darwin", "arm64", false)
			return raw, err
		},
	},
	{
		name: "GitHub release asset", oldLimit: 5 * time.Minute, redirect: true,
		fetch: func(ctx context.Context, baseURL, _ string, _ []byte) ([]byte, error) {
			return downloadReleaseAssetBytes(ctx, githubReleaseAsset{BrowserDownloadURL: baseURL + "/releases/download/v1/agent.zip"})
		},
	},
	{
		name: "Lite firmware", oldLimit: 10 * time.Minute,
		fetch: func(ctx context.Context, baseURL, dir string, _ []byte) ([]byte, error) {
			path, err := downloadFirmwareInto(ctx, &firmwareAsset{Name: "fw.bin", Version: "1", DownloadURL: baseURL + "/fw.bin"}, dir, nil)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(path)
		},
	},
	{
		name: "driver add-on", oldLimit: 10 * time.Minute,
		fetch: func(ctx context.Context, baseURL, dir string, payload []byte) ([]byte, error) {
			sum := sha256.Sum256(payload)
			path, err := downloadDriver(ctx, &agentpbv2.DriverSpec{Name: "nic", ArtifactUrl: baseURL + "/nic.raw", Sha256: hex.EncodeToString(sum[:])}, dir)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(path)
		},
	},
}

// testDownloadPayload is testImagePayload with the zip signature the macOS
// agent artifact is checked for.
func testDownloadPayload(size int) []byte {
	b := testImagePayload(size)
	copy(b, "PK\x03\x04")
	return b
}

// serveDownload serves s to site and returns the server's base URL. A site
// whose real host redirects reaches s through a redirect that differs on
// every request, the way GitHub hands out a fresh signed CDN URL each time.
func serveDownload(t *testing.T, site downloadSite, s *flakyImageServer) string {
	t.Helper()
	var handler http.Handler = s
	if site.redirect {
		var signed atomic.Int32
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/cdn/") {
				s.ServeHTTP(w, r)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/cdn%s?sig=%d", r.URL.Path, signed.Add(1)), http.StatusFound)
		})
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.CloseClientConnections() // a stalled handler would otherwise hold Close
		srv.Close()
	})
	return srv.URL
}

// refuseTransferDeadlines fails the test when a download request carries a
// deadline, which is how http.Client.Timeout bounds a whole exchange.
func refuseTransferDeadlines(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = cliUpdateTransport(func(req *http.Request) (*http.Response, error) {
		if deadline, ok := req.Context().Deadline(); ok {
			t.Errorf("GET %s must finish within %s, however steadily the bytes arrive",
				req.URL.RequestURI(), time.Until(deadline).Round(time.Second))
		}
		return previous.RoundTrip(req)
	})
}

func assertNoDownloadLeftovers(t *testing.T, dir string) {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("failed download left files: %v, %v", files, err)
	}
}

// A slow link that keeps delivering bytes must never time out. Each transfer
// below lasts longer than the site's old whole-transfer limit, scaled down.
func TestOtherDownloadsSlowLinkDoesNotTimeOut(t *testing.T) {
	useTestDownloadPolicy(t, time.Minute/downloadTestScale)
	refuseTransferDeadlines(t)
	for _, site := range downloadSites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel() // the sites wait side by side; the policy above outlives them
			// A write every 5 ms never leaves the link idle for long, and
			// there are enough of them to outlast the old limit.
			const piece, delay = 64, 5 * time.Millisecond
			oldLimit := site.oldLimit / downloadTestScale
			payload := testDownloadPayload(piece * (int(oldLimit/delay) + 4))
			s := &flakyImageServer{payload: payload, ranges: true, piece: piece, delay: delay}
			started := time.Now()
			got, err := site.fetch(context.Background(), serveDownload(t, site, s), t.TempDir(), payload)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("slow but live download failed after %s: %v", elapsed, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("got %d bytes that differ from the %d served", len(got), len(payload))
			}
			if elapsed <= oldLimit {
				t.Fatalf("transfer took %s, which does not outlast the old limit of %s", elapsed, oldLimit)
			}
		})
	}
}

// A connection cut mid-transfer resumes from the last byte, or starts over
// when the server has no range support, and the bytes still come out right.
func TestOtherDownloadsSurviveDroppedConnection(t *testing.T) {
	for _, site := range downloadSites {
		for _, tc := range []struct {
			name   string
			ranges bool
		}{
			{name: "resumes", ranges: true},
			{name: "restarts without range support"},
		} {
			t.Run(site.name+"/"+tc.name, func(t *testing.T) {
				useTestDownloadPolicy(t, 200*time.Millisecond)
				payload := testDownloadPayload(8 << 10)
				s := &flakyImageServer{payload: payload, ranges: tc.ranges, failAt: 5000}
				got, err := site.fetch(context.Background(), serveDownload(t, site, s), t.TempDir(), payload)
				if err != nil {
					t.Fatalf("download did not survive the dropped connection: %v", err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("got %d bytes that differ from the %d served", len(got), len(payload))
				}
				// Asked of both servers; only the one with range support honors it.
				if !s.sawRange("bytes=5000-") {
					t.Fatalf("no request for the missing bytes; saw %q", s.seen)
				}
			})
		}
	}
}

// A server that stops sending is given up on after the idle timeout and a
// bounded number of resumes, with an error that says no data arrived.
func TestOtherDownloadsStallFailsAfterIdleTimeout(t *testing.T) {
	useTestDownloadPolicy(t, 100*time.Millisecond)
	for _, site := range downloadSites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			payload := testDownloadPayload(8 << 10)
			s := &flakyImageServer{payload: payload, ranges: true, failAt: 4096, stall: true, failEvery: true}
			baseURL, dir := serveDownload(t, site, s), t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := site.fetch(ctx, baseURL, dir, payload)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, errDownloadStalled) || !strings.Contains(err.Error(), "no data received for 100ms") {
					t.Fatalf("err = %v, want a stall error that names the idle timeout", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("still waiting on a stalled server after 5s; the idle timeout is 100ms")
			}
			if !s.sawRange("bytes=4096-") {
				t.Fatalf("stalled download was not resumed; saw %q", s.seen)
			}
			assertNoDownloadLeftovers(t, dir)
		})
	}
}

// Ctrl+C (a cancelled context) ends a download at once, whatever the server
// is doing, and leaves no partial file.
func TestOtherDownloadsStopOnCancel(t *testing.T) {
	useTestDownloadPolicy(t, time.Minute) // only the cancellation can end these downloads
	for _, site := range downloadSites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			payload := testDownloadPayload(8 << 10)
			s := &flakyImageServer{payload: payload, ranges: true, failAt: 4096, stall: true, failEvery: true}
			baseURL, dir := serveDownload(t, site, s), t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := site.fetch(ctx, baseURL, dir, payload)
				done <- err
			}()
			time.Sleep(100 * time.Millisecond) // let the body start, then stall
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want the cancellation", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("download did not stop within 2s of the cancellation")
			}
			assertNoDownloadLeftovers(t, dir)
		})
	}
}

// Leaving the firmware progress bar (q or Ctrl+C), or cancelling the command,
// must stop the download: it has no deadline that would end it otherwise.
// Leaving the bar is the person's choice, so it is not reported as an error.
func TestDownloadFirmwareStopsOnProgressExit(t *testing.T) {
	for _, key := range []string{"q", "\x03", "parent"} {
		t.Run(key, func(t *testing.T) {
			useTestDownloadPolicy(t, time.Minute) // only leaving the progress bar can end this download
			s := &flakyImageServer{payload: testDownloadPayload(8 << 10), ranges: true, failAt: 4096, stall: true, failEvery: true}
			baseURL, dir := serveDownload(t, downloadSite{}, s), t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			input, keys := io.Pipe()
			defer input.Close()
			defer keys.Close()
			done := make(chan error, 1)
			go func() {
				_, err := downloadFirmware(ctx, &firmwareAsset{Name: "fw.bin", Version: "1", DownloadURL: baseURL + "/fw.bin"}, dir,
					tea.WithInput(input), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignals())
				done <- err
			}()
			for started := time.Now(); !s.sawRange(""); time.Sleep(5 * time.Millisecond) {
				if time.Since(started) > 3*time.Second {
					t.Fatal("download did not begin")
				}
			}
			if key == "parent" {
				cancel()
			} else if _, err := io.WriteString(keys, key); err != nil {
				t.Fatal(err)
			}
			want := ErrUserCancelled
			if key == "parent" {
				want = context.Canceled
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want %v", err, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("progress exit waited for a download that was not cancelled")
			}
			assertNoDownloadLeftovers(t, dir)
		})
	}
}

// The size limit counts the bytes written before a resume: a body that passes
// the limit only after the connection was cut is still refused.
func TestLiteSimulatorDownloadLimitHoldsAcrossResume(t *testing.T) {
	payload := testDownloadPayload(8 << 10)
	for _, tc := range []struct {
		name      string
		limit     int64
		wantError string
	}{
		{name: "fits exactly", limit: 8 << 10},
		{name: "passes the limit after the resume", limit: 6000, wantError: "download limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTestDownloadPolicy(t, 200*time.Millisecond)
			var cut atomic.Bool
			// No Content-Length, so only the stream can show the size.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := 0
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start); err == nil {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, len(payload)-1))
					w.WriteHeader(http.StatusPartialContent)
				}
				if cut.CompareAndSwap(false, true) {
					_, _ = w.Write(payload[:4096])
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler) // closes the connection mid-body
				}
				_, _ = w.Write(payload[start:])
			}))
			defer srv.Close()
			dir := t.TempDir()
			path, err := downloadLiteSimulatorImage(context.Background(), dir, &imageInfo{DownloadURL: srv.URL + "/firmware.bin", ImageSize: tc.limit})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err = %v, want %q", err, tc.wantError)
				}
				assertNoDownloadLeftovers(t, dir)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("resumed download differs from the %d bytes served: %d bytes, %v", len(payload), len(got), err)
			}
		})
	}
}
