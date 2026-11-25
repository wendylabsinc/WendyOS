package livesensorlink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func metadataPath(m Metadata) string {
	return fmt.Sprintf("/v1/frames/%d/%d/%d/%s", m.ChannelID, m.FrameSequence, m.TimestampUS, m.JPEGSHA256)
}
func captureFresh(c sensorrecord.Capture, now time.Time) bool {
	return c.CapturedUnixNS > 0 && now.UnixNano() >= c.CapturedUnixNS && now.Sub(time.Unix(0, c.CapturedUnixNS)) <= MaxCaptureAge
}
func checkMetadata(m Metadata) (sensorrecord.Capture, error) {
	c, e := sensorrecord.Decode(m.CaptureJSON)
	if e != nil || c.SensorLink != nil || m.Version != 1 || m.ChannelID == 0 || m.FrameSequence == 0 || m.Flags != sensorlink.FlagKeyframe || m.JPEGSize < 1 || m.JPEGSize > sensorlink.MaxFrameBytes || !sensing.DigestValid(m.JPEGSHA256) || c.CapturedUnixNS%1000 != 0 || m.TimestampUS != uint64(c.CapturedUnixNS/1000) {
		return sensorrecord.Capture{}, ErrMetadata
	}
	s, e := c.Sample()
	if e != nil || s.Kind != "camera" || s.Evidence == nil || s.Evidence.ContentType != "image/png" {
		return sensorrecord.Capture{}, ErrMetadata
	}
	return c, nil
}
func commissioned(c sensorrecord.Capture, expected Commission) bool {
	s, e := c.Sample()
	return e == nil && c.SceneBootID == expected.BootID && c.SimulationAnchorUnixNS == expected.AnchorUnixNS && s.RunID == expected.RunID && s.PackageSHA256 == expected.PackageSHA256 && s.SiteID == expected.Site && s.Frame == expected.Frame && s.SensorID == expected.SensorID
}

type cacheEntry struct {
	raw      []byte
	captured int64
	inserted time.Time
}
type metadataCache struct {
	mu       sync.Mutex
	token    string
	now      func() time.Time
	entries  map[string]cacheEntry
	order    []string
	requests chan struct{}
}

func newCache(token string) *metadataCache {
	return &metadataCache{token: token, now: time.Now, entries: map[string]cacheEntry{}, requests: make(chan struct{}, 8)}
}
func (c *metadataCache) put(m Metadata) error {
	capture, e := checkMetadata(m)
	if e != nil {
		return e
	}
	raw, e := json.Marshal(m)
	if e != nil || len(raw) > MaxMetadataBytes {
		return ErrMetadata
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !captureFresh(capture, now) {
		return ErrMetadata
	}
	key := metadataPath(m)
	if old, ok := c.entries[key]; ok {
		if !bytes.Equal(old.raw, raw) {
			return ErrMetadata
		}
		return nil
	}
	for len(c.order) >= MaxCacheFrames {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[key] = cacheEntry{raw: raw, captured: capture.CapturedUnixNS, inserted: now}
	c.order = append(c.order, key)
	return nil
}
func (c *metadataCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case c.requests <- struct{}{}:
		defer func() { <-c.requests }()
	default:
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "read-only endpoint", http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+c.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.RawQuery != "" || r.URL.EscapedPath() != r.URL.Path || !strings.HasPrefix(r.URL.Path, "/v1/frames/") {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	entry, ok := c.entries[r.URL.Path]
	now := c.now()
	if ok && (now.Sub(entry.inserted) < 0 || now.Sub(entry.inserted) > MaxCaptureAge || now.UnixNano() < entry.captured || now.Sub(time.Unix(0, entry.captured)) > MaxCaptureAge) {
		delete(c.entries, r.URL.Path)
		ok = false
	}
	var data []byte
	if ok {
		data = bytes.Clone(entry.raw)
	}
	c.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

type metadataClient struct {
	mu                   sync.Mutex
	url, token           string
	commission           Commission
	http                 *http.Client
	now                  func() time.Time
	lastFrame            uint32
	lastNative           uint64
	lastWall, lastSample int64
	fault                bool
}

func newMetadataClient(c BridgeConfig, token string) *metadataClient {
	return &metadataClient{url: strings.TrimSuffix(c.MetadataURL, "/"), token: token, commission: c.Commission, http: boundedHTTP(time.Duration(c.Bounds.RequestTimeoutMS) * time.Millisecond), now: time.Now}
}
func (c *metadataClient) fetch(ctx context.Context, frame sensorlink.Frame) (capture sensorrecord.Capture, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		if err != nil {
			c.fault = true
		}
	}()
	if c.fault || frame.Seq == 0 || frame.Seq <= c.lastFrame || frame.Flags != sensorlink.FlagKeyframe || len(frame.Payload) == 0 || len(frame.Payload) > sensorlink.MaxFrameBytes {
		return capture, ErrMetadata
	}
	wanted := Metadata{ChannelID: frame.ChannelID, FrameSequence: frame.Seq, TimestampUS: frame.TsUs, JPEGSHA256: digest(frame.Payload)}
	req, e := http.NewRequestWithContext(ctx, "GET", c.url+metadataPath(wanted), nil)
	if e != nil {
		return capture, ErrMetadata
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	response, e := c.http.Do(req)
	if e != nil {
		return capture, ErrMetadata
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" {
		return capture, ErrMetadata
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, MaxMetadataBytes+1))
	if e != nil || len(raw) > MaxMetadataBytes {
		return capture, ErrMetadata
	}
	var metadata Metadata
	if jws.DecodeStrict(raw, &metadata) != nil {
		return capture, ErrMetadata
	}
	if metadata.ChannelID != frame.ChannelID || metadata.FrameSequence != frame.Seq || metadata.TimestampUS != frame.TsUs || metadata.Flags != frame.Flags || metadata.JPEGSHA256 != wanted.JPEGSHA256 || metadata.JPEGSize != int64(len(frame.Payload)) {
		return capture, ErrMetadata
	}
	capture, e = checkMetadata(metadata)
	if e != nil || !commissioned(capture, c.commission) || !captureFresh(capture, c.now()) {
		return sensorrecord.Capture{}, ErrMetadata
	}
	s, _ := capture.Sample()
	if c.lastFrame != 0 && (s.Sequence <= c.lastNative || s.SampleNS <= c.lastSample || capture.CapturedUnixNS <= c.lastWall) {
		return sensorrecord.Capture{}, ErrMetadata
	}
	c.lastFrame, c.lastNative, c.lastWall, c.lastSample = frame.Seq, s.Sequence, capture.CapturedUnixNS, s.SampleNS
	return capture, nil
}
