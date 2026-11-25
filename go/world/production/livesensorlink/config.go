package livesensorlink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"golang.org/x/sys/unix"
)

func (c Commission) validate() error {
	if c.RunID == "" || len(c.RunID) > 80 || c.BootID == "" || len(c.BootID) > 128 || !sensing.DigestValid(c.PackageSHA256) || c.Site == "" || len(c.Site) > 256 || c.Frame.Validate() != nil || c.SensorID != "camera-b" || c.AnchorUnixNS <= 0 || c.AnchorUnixNS > int64(^uint64(0)>>1)-int64(24*time.Hour) {
		return ErrConfig
	}
	for _, s := range []string{c.RunID, c.BootID, c.Site} {
		if strings.TrimSpace(s) != s || strings.ContainsAny(s, "\r\n\x00") {
			return ErrConfig
		}
	}
	return nil
}
func (b Bounds) normalize() (Bounds, error) {
	if b.RequestTimeoutMS == 0 {
		b.RequestTimeoutMS = 1500
	}
	if b.TTLMS == 0 {
		b.TTLMS = 3000
	}
	if b.DurationMS < 1 || b.DurationMS > 600000 || b.MaxFrames < 1 || b.MaxFrames > 64 || b.RequestTimeoutMS < 10 || b.RequestTimeoutMS > 3000 || b.TTLMS < 1000 || b.TTLMS > 10000 {
		return b, ErrConfig
	}
	return b, nil
}
func address(s string, listen bool) bool {
	host, port, e := net.SplitHostPort(s)
	if e != nil || (!listen && host == "") || strings.ContainsAny(host, "/\r\n\x00") {
		return false
	}
	n, e := strconv.ParseUint(port, 10, 16)
	return e == nil && n > 0 && strconv.FormatUint(n, 10) == port
}
func origin(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "http" && u.User == nil && u.Opaque == "" && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && address(u.Host, false)
}
func (c SourceConfig) normalize() (SourceConfig, error) {
	var e error
	c.Bounds, e = c.Bounds.normalize()
	if e != nil {
		return c, e
	}
	if c.ChunkBytes == 0 {
		c.ChunkBytes = 16384
	}
	if c.JPEGQuality == 0 {
		c.JPEGQuality = 80
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 1000
	}
	if !c.IsolatedSimulationNetwork || c.Commission.validate() != nil || !origin(c.SceneURL) || !address(c.TCPListen, true) || !address(c.MetadataListen, true) || c.SceneReadTokenFile == "" || c.MetadataReadTokenFile == "" || c.ChannelID == 0 || c.ChunkBytes < 64 || c.ChunkBytes > 64<<10 || c.JPEGQuality < 1 || c.JPEGQuality > 100 || c.PollIntervalMS < 50 || c.PollIntervalMS > 10000 {
		return c, ErrConfig
	}
	if c.Drop != nil && (c.Drop.FrameSequence < 1 || c.Drop.FrameSequence > uint32(c.Bounds.MaxFrames) || c.Drop.ChunkSequence >= 131072) {
		return c, ErrConfig
	}
	return c, nil
}
func (c BridgeConfig) normalize() (BridgeConfig, error) {
	var e error
	c.Bounds, e = c.Bounds.normalize()
	if e != nil {
		return c, e
	}
	if !c.IsolatedSimulationNetwork || c.Commission.validate() != nil || !address(c.TCPAddress, false) || !origin(c.MetadataURL) || c.MetadataReadTokenFile == "" || c.ChannelID == 0 {
		return c, ErrConfig
	}
	return c, nil
}

func readFile(path string, limit int, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConfig
	}
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, ErrConfig
	}
	f := os.NewFile(uintptr(fd), "live-sensorlink-config")
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || private && st.Mode().Perm()&0077 != 0 || st.Size() < 1 || st.Size() > int64(limit) {
		return nil, ErrConfig
	}
	b, e := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	if e != nil || len(b) > limit {
		return nil, ErrConfig
	}
	return b, nil
}
func readToken(path string) (string, error) {
	b, e := readFile(path, 257, true)
	if e != nil {
		return "", e
	}
	defer clear(b)
	b = bytes.TrimSuffix(b, []byte("\n"))
	if len(b) < 32 || len(b) > 256 {
		return "", ErrConfig
	}
	for _, r := range b {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", ErrConfig
		}
	}
	return string(b), nil
}
func LoadSourceConfig(path string) (SourceConfig, error) {
	var c SourceConfig
	b, e := readFile(path, 16384, false)
	if e != nil || jws.DecodeStrict(b, &c) != nil {
		return c, ErrConfig
	}
	return c.normalize()
}
func LoadBridgeConfig(path string) (BridgeConfig, error) {
	var c BridgeConfig
	b, e := readFile(path, 16384, false)
	if e != nil || jws.DecodeStrict(b, &c) != nil {
		return c, ErrConfig
	}
	return c.normalize()
}
func boundedHTTP(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: timeout}).DialContext, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2, IdleConnTimeout: timeout, ResponseHeaderTimeout: timeout, MaxResponseHeaderBytes: 4096}, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrMetadata }}
}
func resultError(r *Result, err error) {
	if err == nil {
		return
	}
	r.Status = "failed"
	switch {
	case errors.Is(err, liveapp.ErrUncertain):
		r.Status = "unknown_outcome"
		r.ErrorCode = "mutation_outcome_unknown"
	case errors.Is(err, ErrConfig):
		r.ErrorCode = "configuration_refused"
	case errors.Is(err, livecapture.ErrBootChanged):
		r.ErrorCode = "scene_boot_changed"
	case errors.Is(err, livecapture.ErrStale):
		r.ErrorCode = "capture_stale"
	case errors.Is(err, context.DeadlineExceeded):
		r.ErrorCode = "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		r.ErrorCode = "cancelled"
	case errors.Is(err, ErrMetadata):
		r.ErrorCode = "frame_metadata_refused"
	default:
		r.ErrorCode = "capture_or_transport_refused"
	}
}
