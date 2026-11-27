package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Multi-gigabyte OS images, flashpacks and OTA artifacts take hours on a slow
// link, and a slow enough link outlasts any fixed limit on the smaller
// downloads too (agent, firmware, simulator image, driver add-on), so no
// download carries a whole-transfer deadline. An attempt fails only when no
// byte arrives for idleTimeout, and the next attempt asks for the missing
// bytes with a Range request instead of starting over.
type downloadPolicy struct {
	// idleTimeout aborts an attempt that receives no response headers or
	// body bytes for this long.
	idleTimeout time.Duration
	// maxRetries bounds consecutive failed attempts that get no further than
	// the furthest byte already reached; any attempt that does resets it.
	maxRetries int
	// retryDelay is the first backoff, doubled per consecutive failure up to
	// maxRetryDelay.
	retryDelay, maxRetryDelay time.Duration
}

// imageDownloadPolicy is a variable so tests can shrink it.
var imageDownloadPolicy = downloadPolicy{
	idleTimeout:   60 * time.Second,
	maxRetries:    6,
	retryDelay:    2 * time.Second,
	maxRetryDelay: 30 * time.Second,
}

var (
	errDownloadStalled   = errors.New("no data received")
	errRangesUnsupported = errors.New("server ignored a range request")
	errDownloadTooLarge  = errors.New("exceeds download limit")
)

// downloadStatusError is the HTTP status that failed a download. It is a type
// so a caller can name its own artifact in the message.
type downloadStatusError int

func (e downloadStatusError) Error() string {
	return fmt.Sprintf("download returned status %d", int(e))
}

// downloadDest receives a download: a file, or a downloadBuffer for a small
// artifact that is checked and used in memory.
type downloadDest interface {
	io.WriterAt
	Truncate(size int64) error
}

// downloadBuffer is a downloadDest in memory. A whole-resource download writes
// in order, so it only ever appends or starts over.
type downloadBuffer []byte

func (b *downloadBuffer) WriteAt(p []byte, off int64) (int, error) {
	if off != int64(len(*b)) {
		return 0, fmt.Errorf("out-of-order write at byte %d of %d", off, len(*b))
	}
	*b = append(*b, p...)
	return len(p), nil
}

func (b *downloadBuffer) Truncate(size int64) error {
	if size < 0 || size > int64(len(*b)) {
		return fmt.Errorf("cannot resize a %d-byte download to %d bytes", len(*b), size)
	}
	*b = (*b)[:size]
	return nil
}

// downloadResumable downloads the whole of url into dst the way an OS image is
// downloaded: no deadline on the transfer, an idle timeout on every attempt,
// and a resume after a dropped connection, or a restart from zero when the
// server has no range support. maxBytes > 0 refuses a larger resource: by its
// declared size before any byte is read, else once the body passes the limit.
// progress may be nil.
func downloadResumable(ctx context.Context, url string, dst downloadDest, maxBytes int64, progress func(downloaded, total int64)) error {
	if progress == nil {
		progress = func(int64, int64) {}
	}
	// No Timeout, as in downloadToFile: idleTimeout bounds every request.
	f := &resumableFetch{client: &http.Client{}, url: url, dst: dst, end: -1, wholeFile: true, size: -1, limit: maxBytes, progress: progress}
	return f.run(ctx)
}

// fetchResumable writes the bytes [start, end] of url into dst at their own
// offsets; end < 0 means through the end of the resource. size is the
// resource's length when already known (from a HEAD), else -1. A dropped
// connection, a 408/429/5xx or an idle timeout fails only the attempt: the
// next one, after a backoff, resumes from the last byte written. When the whole
// resource was asked for and the server ignores the Range, the body restarts
// from zero; for a partial span that is errRangesUnsupported. progress gets the
// next offset and the resource size (-1 if unknown) after every write.
func fetchResumable(ctx context.Context, client *http.Client, url string, dst downloadDest, start, end, size int64, progress func(offset, size int64)) error {
	f := &resumableFetch{client: client, url: url, dst: dst, end: end, wholeFile: start == 0 && end < 0, offset: start, size: size, progress: progress}
	return f.run(ctx)
}

// run makes attempts until the wanted bytes are in f.dst or the policy gives up.
func (f *resumableFetch) run(ctx context.Context) error {
	p := imageDownloadPolicy
	furthest, failures := f.offset, 0
	for {
		retry, err := f.attempt(ctx, p.idleTimeout)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err == nil || !retry {
			return err
		}
		if f.offset > furthest {
			furthest, failures = f.offset, 0
		} else {
			failures++
			if failures > p.maxRetries {
				return fmt.Errorf("%w (gave up after %d retries without progress at byte %d)", err, p.maxRetries, f.offset)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(p.retryDelay<<failures, p.maxRetryDelay)):
		}
	}
}

type resumableFetch struct {
	client    *http.Client
	url       string
	dst       downloadDest
	end       int64 // last byte wanted, or -1 for the end of the resource
	wholeFile bool  // asked for the whole resource, so a 200 can restart it
	offset    int64 // next byte to write
	size      int64 // resource length once known, else -1
	limit     int64 // largest resource accepted, or 0 for any size
	validator string
	progress  func(offset, size int64)
}

// attempt makes one request from f.offset. retry reports whether err is
// transient; a nil err means the wanted bytes are all in dst.
func (f *resumableFetch) attempt(ctx context.Context, idle time.Duration) (retry bool, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(idle, func() { cancel(errDownloadStalled) })
	defer watchdog.Stop()
	stalled := func(err error) error {
		if errors.Is(context.Cause(ctx), errDownloadStalled) {
			return fmt.Errorf("%w for %s", errDownloadStalled, idle)
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return false, err
	}
	ranged := f.offset > 0 || f.end >= 0
	if ranged {
		rng := fmt.Sprintf("bytes=%d-", f.offset)
		if f.end >= 0 {
			rng += strconv.FormatInt(f.end, 10)
		}
		req.Header.Set("Range", rng)
		if f.validator != "" {
			req.Header.Set("If-Range", f.validator)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return true, stalled(err)
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusPartialContent && ranged:
		if err := f.checkContentRange(resp.Header.Get("Content-Range")); err != nil {
			return false, err
		}
	case code == http.StatusOK && f.wholeFile:
		// The first response, or a server that ignored the Range (no range
		// support, or If-Range saw a changed file): take the body from zero.
		if f.offset > 0 {
			if err := f.dst.Truncate(0); err != nil {
				return false, err
			}
			f.offset = 0
		}
		f.size = resp.ContentLength
		f.validator = rangeValidator(resp.Header)
	case code == http.StatusOK:
		return false, errRangesUnsupported
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return true, downloadStatusError(code)
	default:
		return false, downloadStatusError(code)
	}

	var body io.Reader = resp.Body
	if f.limit > 0 {
		if f.size > f.limit {
			return false, fmt.Errorf("Content-Length %d %w of %d bytes", f.size, errDownloadTooLarge, f.limit)
		}
		// One byte past the limit is enough to see that the body exceeds it.
		body = io.LimitReader(body, f.limit-f.offset+1)
	}
	buf := make([]byte, 1*1024*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			watchdog.Reset(idle)
			if f.limit > 0 && f.offset+int64(n) > f.limit {
				return false, fmt.Errorf("%w of %d bytes", errDownloadTooLarge, f.limit)
			}
			if _, err := f.dst.WriteAt(buf[:n], f.offset); err != nil {
				return false, fmt.Errorf("writing at offset %d: %w", f.offset, err)
			}
			f.offset += int64(n)
			f.progress(f.offset, f.size)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return true, stalled(readErr)
		}
	}
	want := f.size
	if f.end >= 0 {
		want = f.end + 1
	}
	if want >= 0 && f.offset < want {
		return true, fmt.Errorf("connection closed at byte %d of %d: %w", f.offset, want, io.ErrUnexpectedEOF)
	}
	return false, nil
}

// checkContentRange accepts a 206 only when it starts at the byte asked for
// and describes a resource of the size already known. A missing header is
// tolerated, as it was before resuming existed.
func (f *resumableFetch) checkContentRange(header string) error {
	if header == "" {
		return nil
	}
	rng, total, ok := strings.Cut(strings.TrimPrefix(header, "bytes "), "/")
	first, _, _ := strings.Cut(rng, "-")
	start, err := strconv.ParseInt(first, 10, 64)
	if !ok || err != nil {
		return fmt.Errorf("malformed Content-Range %q", header)
	}
	if start != f.offset {
		return fmt.Errorf("server resumed at byte %d, want %d", start, f.offset)
	}
	if size, err := strconv.ParseInt(total, 10, 64); err == nil {
		if f.size >= 0 && size != f.size {
			return fmt.Errorf("server reports %d bytes, expected %d; the file changed during the download", size, f.size)
		}
		f.size = size
	}
	return nil
}

// rangeValidator is the If-Range value that keeps a resume on the same file:
// a strong ETag, else Last-Modified.
func rangeValidator(h http.Header) string {
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}
	return h.Get("Last-Modified")
}
