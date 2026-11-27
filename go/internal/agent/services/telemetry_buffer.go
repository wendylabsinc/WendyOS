package services

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// SignalType identifies a telemetry signal kind.
type SignalType string

const (
	SignalLogs    SignalType = "logs"
	SignalMetrics SignalType = "metrics"
	SignalTraces  SignalType = "traces"
)

const maxSegmentFrameBytes = 1 * 1024 * 1024 // 1 MB per frame; single OTLP batch upper bound

// ErrCorruptSegment is returned by readFramesFrom when a frame length prefix
// exceeds maxSegmentFrameBytes, indicating a corrupt or truncated segment.
// Callers should skip to the next segment rather than retrying the same offset.
var ErrCorruptSegment = errors.New("telemetry buffer: corrupt segment frame length")

// segmentFilename returns the filename for a segment file, e.g. "logs-000001.bin".
func segmentFilename(signal SignalType, seqNum int) string {
	return fmt.Sprintf("%s-%06d.bin", signal, seqNum)
}

// listSegments returns segment filenames (basename only) for signal, sorted
// ascending by sequence number. It does not prepend the directory.
func listSegments(dir string, signal SignalType) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := string(signal) + "-"
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".bin") {
			names = append(names, n)
		}
	}
	sort.Strings(names) // zero-padded seq → lexicographic = numeric order
	return names, nil
}

// newProtoForSignal allocates a fresh proto.Message of the correct type for signal.
func newProtoForSignal(signal SignalType) proto.Message {
	switch signal {
	case SignalLogs:
		return &collogspb.ExportLogsServiceRequest{}
	case SignalMetrics:
		return &colmetricspb.ExportMetricsServiceRequest{}
	case SignalTraces:
		return &coltracepb.ExportTraceServiceRequest{}
	default:
		return nil
	}
}

// readFramesFrom reads up to maxN length-prefixed protobuf frames from path
// starting at startOffset. Returns the decoded messages, the byte offset after
// the last successfully read frame, and any I/O error (os.ErrNotExist included).
// A partial frame or unmarshal error stops reading cleanly; no partial data is returned.
func readFramesFrom(path string, startOffset int64, signal SignalType, maxN int) ([]proto.Message, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, startOffset, err
	}
	defer f.Close()

	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return nil, startOffset, err
		}
	}

	offset := startOffset
	var msgs []proto.Message

	for len(msgs) < maxN {
		// Allocate the destination proto first so we catch unknown signals before I/O.
		msg := newProtoForSignal(signal)
		if msg == nil {
			break
		}
		var hdr [4]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				return msgs, offset, err
			}
			break
		}
		length := binary.BigEndian.Uint32(hdr[:])
		if length > maxSegmentFrameBytes {
			return msgs, offset, ErrCorruptSegment
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(f, data); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				return msgs, offset, err
			}
			break
		}
		if err := proto.Unmarshal(data, msg); err != nil {
			break // corrupted frame — stop reading this file cleanly
		}
		msgs = append(msgs, msg)
		offset += int64(4 + length)
	}

	return msgs, offset, nil
}

const (
	defaultMaxTotalBytes int64 = 100 * 1024 * 1024 // 100 MB
	defaultSegmentBytes  int64 = 4 * 1024 * 1024   // 4 MB
	defaultTelemetryDir        = "/var/lib/wendy-agent/telemetry"
	// maxReplayFrames caps last_n replay requests to prevent a malicious or
	// misconfigured client from driving unbounded disk reads per stream open.
	maxReplayFrames = 1000
)

// TelemetryBufferConfig configures TelemetryBuffer storage limits.
type TelemetryBufferConfig struct {
	Dir           string // defaults to $WENDY_TELEMETRY_DIR or /var/lib/wendy-agent/telemetry
	MaxTotalBytes int64  // defaults to 100 MB
	SegmentBytes  int64  // defaults to 4 MB
}

func (c *TelemetryBufferConfig) applyDefaults() {
	if c.Dir == "" {
		if dir := os.Getenv("WENDY_TELEMETRY_DIR"); dir != "" {
			c.Dir = dir
		} else {
			c.Dir = defaultTelemetryDir
		}
	}
	if c.MaxTotalBytes == 0 {
		c.MaxTotalBytes = defaultMaxTotalBytes
	}
	if c.SegmentBytes == 0 {
		c.SegmentBytes = defaultSegmentBytes
	}
}

// flushCursor is the in-memory flush position for a single signal type.
// It is NOT written to disk directly; the on-disk format is cursorState,
// which adds an HMAC-SHA256 integrity field. See SaveCursor/LoadCursor.
type flushCursor struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
}

type segWriter struct {
	f      *os.File
	size   int64
	seqNum int
}

// TelemetryBuffer persists OTel telemetry to rotating segment files and
// fans out to an in-memory TelemetryBroadcaster.
//
// Segment files are protected at the OS level: the storage directory is mode
// 0700 and individual files are mode 0600. cursor.json integrity is enforced
// by HMAC-SHA256 using a device-stable key stored in cursor.key (mode 0400).
type TelemetryBuffer struct {
	cfg         TelemetryBufferConfig
	broadcaster *TelemetryBroadcaster
	logger      *zap.Logger
	mu          sync.Mutex
	cursorMu    sync.Mutex // protects cursor.json read-modify-write
	writers     map[SignalType]*segWriter
	hmacKey     []byte                     // 32-byte HMAC key from cursor.key; nil disables disk cursor
	memCursor   map[SignalType]flushCursor // in-memory cursor fallback when hmacKey is nil
}

// NewTelemetryBuffer creates a TelemetryBuffer, creating the storage directory
// if needed. Falls back gracefully to in-memory-only mode if the directory
// cannot be created; check DiskEnabled() to distinguish the two modes.
// A nil *TelemetryBuffer is a no-op for all methods.
func NewTelemetryBuffer(cfg TelemetryBufferConfig, broadcaster *TelemetryBroadcaster, logger *zap.Logger) *TelemetryBuffer {
	cfg.applyDefaults()

	b := &TelemetryBuffer{
		cfg:         cfg,
		broadcaster: broadcaster,
		logger:      logger,
		writers:     make(map[SignalType]*segWriter),
		memCursor:   make(map[SignalType]flushCursor),
	}

	// For production paths, resolve symlinks on the PARENT directory before
	// MkdirAll. This closes the TOCTOU window where a symlink under
	// /var/lib/wendy-agent/ could redirect writes after Clean but before Create.
	if strings.HasPrefix(cfg.Dir+"/", "/var/lib/wendy-agent/") {
		parent := filepath.Dir(cfg.Dir)
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil || !strings.HasPrefix(resolvedParent+"/", "/var/lib/wendy-agent/") {
			logger.Warn("telemetry buffer: dir parent resolves outside allowed prefix, disk writes disabled",
				zap.String("dir", cfg.Dir))
			return b
		}
		cfg.Dir = filepath.Join(resolvedParent, filepath.Base(cfg.Dir))
		b.cfg.Dir = cfg.Dir
	}

	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		logger.Warn("telemetry buffer: cannot create dir, disk writes disabled", zap.Error(err))
		return b
	}
	// Enforce permissions on pre-existing directories: MkdirAll does not fix
	// the mode if the directory already exists.
	_ = os.Chmod(cfg.Dir, 0700)

	// Re-validate after MkdirAll in case the directory itself was created as a symlink.
	if strings.HasPrefix(cfg.Dir+"/", "/var/lib/wendy-agent/") {
		resolved, err := filepath.EvalSymlinks(cfg.Dir)
		if err != nil || !strings.HasPrefix(resolved+"/", "/var/lib/wendy-agent/") {
			logger.Warn("telemetry buffer: dir resolves outside allowed prefix, disk writes disabled",
				zap.String("dir", cfg.Dir))
			return b
		}
		b.cfg.Dir = resolved
	}

	b.hmacKey = loadOrGenerateCursorKey(b.cfg.Dir, logger)

	b.evictIfNeeded()

	for _, sig := range []SignalType{SignalLogs, SignalMetrics, SignalTraces} {
		if err := b.openLatestWriter(sig); err != nil {
			logger.Warn("telemetry buffer: cannot open writer, disk writes disabled",
				zap.String("signal", string(sig)), zap.Error(err))
		}
	}

	return b
}

func (b *TelemetryBuffer) openLatestWriter(sig SignalType) error {
	segs, _ := listSegments(b.cfg.Dir, sig)
	seqNum := 1
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		trimmed := strings.TrimPrefix(last, string(sig)+"-")
		trimmed = strings.TrimSuffix(trimmed, ".bin")
		if n, err := fmt.Sscanf(trimmed, "%d", &seqNum); n != 1 || err != nil {
			seqNum = len(segs) + 1
		}
	}

	path := filepath.Join(b.cfg.Dir, segmentFilename(sig, seqNum))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	fi, _ := f.Stat()
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	b.writers[sig] = &segWriter{f: f, size: size, seqNum: seqNum}
	return nil
}

func (b *TelemetryBuffer) rotateWriter(sig SignalType) error {
	w := b.writers[sig]
	seqNum := 1
	if w != nil {
		if err := w.f.Close(); err != nil {
			return fmt.Errorf("telemetry buffer: closing segment: %w", err)
		}
		seqNum = w.seqNum + 1
	}
	b.writers[sig] = nil // cleared so a subsequent open failure leaves a known state
	path := filepath.Join(b.cfg.Dir, segmentFilename(sig, seqNum))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	b.writers[sig] = &segWriter{f: f, size: 0, seqNum: seqNum}
	b.evictIfNeeded()
	return nil
}

// evictIfNeeded removes oldest segment files until total disk use is within
// MaxTotalBytes. Callers must hold b.mu, except during NewTelemetryBuffer
// initialization where no concurrent writers exist yet.
func (b *TelemetryBuffer) evictIfNeeded() {
	total := b.totalSegmentSize()
	for total > b.cfg.MaxTotalBytes {
		oldest := b.findOldestSegment()
		if oldest == "" {
			break
		}
		if err := os.Remove(filepath.Join(b.cfg.Dir, oldest)); err != nil && !os.IsNotExist(err) {
			b.logger.Warn("telemetry buffer: failed to evict segment", zap.String("file", oldest), zap.Error(err))
			break
		}
		total = b.totalSegmentSize()
	}
	if total > b.cfg.MaxTotalBytes {
		b.logger.Warn("telemetry buffer: cannot evict below quota; only active segments remain",
			zap.Int64("total_bytes", total),
			zap.Int64("max_bytes", b.cfg.MaxTotalBytes))
	}
}

func (b *TelemetryBuffer) totalSegmentSize() int64 {
	var total int64
	entries, _ := os.ReadDir(b.cfg.Dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bin") {
			fi, _ := e.Info()
			if fi != nil {
				total += fi.Size()
			}
		}
	}
	return total
}

func (b *TelemetryBuffer) findOldestSegment() string {
	type candidate struct {
		name  string
		mtime int64
	}
	var candidates []candidate
	for _, sig := range []SignalType{SignalLogs, SignalMetrics, SignalTraces} {
		segs, _ := listSegments(b.cfg.Dir, sig)
		w := b.writers[sig]
		activeSeq := -1
		if w != nil {
			activeSeq = w.seqNum
		}
		for _, s := range segs {
			var seq int
			fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(s, string(sig)+"-"), ".bin"), "%d", &seq)
			if seq != activeSeq {
				var mtime int64
				if fi, err := os.Stat(filepath.Join(b.cfg.Dir, s)); err == nil {
					mtime = fi.ModTime().UnixNano()
				}
				candidates = append(candidates, candidate{name: s, mtime: mtime})
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].mtime < candidates[j].mtime
	})
	return candidates[0].name
}

func (b *TelemetryBuffer) writeFrame(sig SignalType, msg proto.Message) {
	data, err := proto.Marshal(msg)
	if err != nil {
		b.logger.Warn("telemetry buffer: marshal failed", zap.Error(err))
		return
	}
	if uint32(len(data)) > maxSegmentFrameBytes {
		b.logger.Warn("telemetry buffer: frame exceeds max size, dropping", zap.Int("size", len(data)))
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	w := b.writers[sig]
	if w == nil {
		return // directory unavailable
	}

	needed := int64(4 + len(data))
	if w.size > 0 && w.size+needed > b.cfg.SegmentBytes {
		if err := b.rotateWriter(sig); err != nil {
			b.logger.Warn("telemetry buffer: rotation failed", zap.String("signal", string(sig)), zap.Error(err))
			return
		}
		w = b.writers[sig]
		if w == nil {
			return
		}
	}

	// Write header and payload in one syscall to reduce the chance of torn frames on crash.
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	if _, err := w.f.Write(frame); err != nil {
		b.logger.Warn("telemetry buffer: write frame failed", zap.Error(err))
		b.rotateWriter(sig) //nolint:errcheck
		return
	}
	w.size += needed
	b.evictIfNeeded()
}

// PublishLogs writes req to disk then fans out to the broadcaster.
func (b *TelemetryBuffer) PublishLogs(req *collogspb.ExportLogsServiceRequest) {
	if b == nil {
		return
	}
	b.writeFrame(SignalLogs, req)
	b.broadcaster.PublishLogs(req)
}

// PublishMetrics writes req to disk then fans out to the broadcaster.
func (b *TelemetryBuffer) PublishMetrics(req *colmetricspb.ExportMetricsServiceRequest) {
	if b == nil {
		return
	}
	b.writeFrame(SignalMetrics, req)
	b.broadcaster.PublishMetrics(req)
}

// PublishTraces writes req to disk then fans out to the broadcaster.
func (b *TelemetryBuffer) PublishTraces(req *coltracepb.ExportTraceServiceRequest) {
	if b == nil {
		return
	}
	b.writeFrame(SignalTraces, req)
	b.broadcaster.PublishTraces(req)
}

// DiskEnabled reports whether at least one signal has an active disk writer.
// Returns false when the storage directory could not be created or validated,
// or when all writers failed during a segment rotation.
func (b *TelemetryBuffer) DiskEnabled() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.writers {
		if w != nil {
			return true
		}
	}
	return false
}

const cursorFile = "cursor.json"

type cursorMap map[SignalType]flushCursor

// cursorState is the on-disk JSON format for cursor.json. It wraps a
// cursorMap with an HMAC-SHA256 integrity field so that write corruption or
// partial-write is detected on load (the HMAC key is device-local; on-device
// attackers with filesystem access can bypass this). The MAC covers the
// JSON-encoded Cursors field; a mismatch causes LoadCursor to reset.
// The HMAC key is stored separately in cursor.key.
type cursorState struct {
	Cursors cursorMap `json:"cursors"`
	HMAC    string    `json:"hmac"`
}

func cursorStateHMAC(key []byte, m cursorMap) (string, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// loadOrGenerateCursorKey loads or creates the 32-byte HMAC key used to
// authenticate cursor.json. Returns nil (and logs a warning) if the key
// cannot be read or generated.
func loadOrGenerateCursorKey(dir string, logger *zap.Logger) []byte {
	keyPath := filepath.Join(dir, "cursor.key")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) == 32 {
		return data
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		logger.Warn("telemetry buffer: cannot generate cursor key, cursor integrity disabled", zap.Error(err))
		return nil
	}
	if err := os.WriteFile(keyPath, key, 0400); err != nil {
		logger.Warn("telemetry buffer: cannot persist cursor key, cursor integrity disabled", zap.Error(err))
		return nil
	}
	return key
}

// LoadCursor returns the persisted flush cursor for sig, or a zero cursor.
// When the HMAC key is unavailable, returns the in-memory cursor so that
// CloudFlusher makes forward progress within a single process lifetime.
func (b *TelemetryBuffer) LoadCursor(sig SignalType) flushCursor {
	if b == nil {
		return flushCursor{}
	}
	if b.hmacKey == nil {
		b.cursorMu.Lock()
		defer b.cursorMu.Unlock()
		return b.memCursor[sig]
	}
	b.cursorMu.Lock()
	defer b.cursorMu.Unlock()
	data, err := os.ReadFile(filepath.Join(b.cfg.Dir, cursorFile))
	if err != nil {
		return flushCursor{}
	}
	var state cursorState
	if err := json.Unmarshal(data, &state); err != nil || state.Cursors == nil {
		b.logger.Warn("telemetry buffer: corrupt cursor.json, resetting")
		return flushCursor{}
	}
	want, cerr := cursorStateHMAC(b.hmacKey, state.Cursors)
	if cerr != nil || want != state.HMAC {
		b.logger.Warn("telemetry buffer: cursor.json HMAC mismatch, resetting")
		return flushCursor{}
	}
	return state.Cursors[sig]
}

// SaveCursor persists cursor for sig to cursor.json atomically.
// When the HMAC key is unavailable, falls back to the in-memory cursor so
// that CloudFlusher makes forward progress within a single process lifetime.
func (b *TelemetryBuffer) SaveCursor(sig SignalType, cursor flushCursor) error {
	if b == nil {
		return nil
	}
	if b.hmacKey == nil {
		b.cursorMu.Lock()
		b.memCursor[sig] = cursor
		b.cursorMu.Unlock()
		return nil
	}
	b.cursorMu.Lock()
	defer b.cursorMu.Unlock()
	path := filepath.Join(b.cfg.Dir, cursorFile)
	var m cursorMap
	if data, err := os.ReadFile(path); err == nil {
		var state cursorState
		if jerr := json.Unmarshal(data, &state); jerr == nil && state.Cursors != nil {
			if want, cerr := cursorStateHMAC(b.hmacKey, state.Cursors); cerr == nil && want == state.HMAC {
				m = state.Cursors
			} else {
				b.logger.Warn("telemetry buffer: cursor.json HMAC mismatch on save, resetting")
			}
		} else {
			b.logger.Warn("telemetry buffer: corrupt cursor.json on save, resetting")
		}
	}
	if m == nil {
		m = make(cursorMap)
	}
	m[sig] = cursor
	mac, err := cursorStateHMAC(b.hmacKey, m)
	if err != nil {
		return err
	}
	data, err := json.Marshal(cursorState{Cursors: m, HMAC: mac})
	if err != nil {
		return err
	}
	// Use os.CreateTemp so concurrent callers write to distinct temp files.
	tmp, err := os.CreateTemp(b.cfg.Dir, "cursor-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // no-op if rename succeeded
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// ReadLastN returns up to n recent entries for sig in ascending time order.
// It reads segment files newest-to-oldest and prepends older frames.
// n is capped at maxReplayFrames to bound per-call disk I/O.
func (b *TelemetryBuffer) ReadLastN(sig SignalType, n int) []proto.Message {
	return b.ReadLastNMatching(sig, n, nil)
}

// ReadLastNMatching returns up to the last n entries for sig that match, in
// ascending time order. match maps a frame to the (possibly reduced) message
// to return, or nil to drop the frame; a nil match keeps every frame as-is.
// Counting matching frames (rather than matching the last n frames overall)
// keeps tail semantics per-filter: a chatty co-tenant cannot push the
// requested entries out of the window. Segments are read newest-to-oldest and
// reading stops once n matches are collected, so sparse matches cost at most
// one pass over the buffer's bounded retention.
func (b *TelemetryBuffer) ReadLastNMatching(sig SignalType, n int, match func(proto.Message) proto.Message) []proto.Message {
	if b == nil {
		return nil
	}
	if n <= 0 {
		return nil
	}
	if n > maxReplayFrames {
		n = maxReplayFrames
	}
	segs, _ := listSegments(b.cfg.Dir, sig)
	var result []proto.Message
	for i := len(segs) - 1; i >= 0 && len(result) < n; i-- {
		// Read all frames in the segment so we can correctly take the trailing ones.
		frames, _, _ := readFramesFrom(filepath.Join(b.cfg.Dir, segs[i]), 0, sig, math.MaxInt32)
		if match != nil {
			matched := make([]proto.Message, 0, len(frames))
			for _, f := range frames {
				if m := match(f); m != nil {
					matched = append(matched, m)
				}
			}
			frames = matched
		}
		need := n - len(result)
		if len(frames) > need {
			frames = frames[len(frames)-need:]
		}
		result = append(frames, result...) // prepend older frames to maintain ascending order
	}
	return result
}

// TelemetryPublisher is the minimal interface for publishing OTel telemetry.
// Both *TelemetryBroadcaster and *TelemetryBuffer implement it.
type TelemetryPublisher interface {
	PublishLogs(req *collogspb.ExportLogsServiceRequest)
	PublishMetrics(req *colmetricspb.ExportMetricsServiceRequest)
	PublishTraces(req *coltracepb.ExportTraceServiceRequest)
}

var _ TelemetryPublisher = (*TelemetryBroadcaster)(nil)
var _ TelemetryPublisher = (*TelemetryBuffer)(nil)

// ReadFromCursor reads up to maxN frames starting at cursor for sig.
// Returns frames, the updated cursor, and any I/O error.
// If cursor.File is empty, reads from the oldest segment.
// If cursor.File was evicted, falls back to current oldest.
func (b *TelemetryBuffer) ReadFromCursor(sig SignalType, cursor flushCursor, maxN int) ([]proto.Message, flushCursor, error) {
	if b == nil {
		return nil, cursor, nil
	}
	segs, err := listSegments(b.cfg.Dir, sig)
	if err != nil || len(segs) == 0 {
		return nil, cursor, err
	}

	startFile := cursor.File
	startOffset := cursor.Offset

	if startFile == "" {
		startFile = segs[0]
		startOffset = 0
	}

	startIdx := 0
	found := false
	for i, s := range segs {
		if s == startFile {
			startIdx = i
			found = true
			break
		}
	}
	if !found {
		// Cursor file evicted — start from current oldest.
		startIdx = 0
		startFile = segs[0]
		startOffset = 0
	}

	var msgs []proto.Message
	currentIdx := startIdx
	currentFile := startFile
	currentOffset := startOffset

	for len(msgs) < maxN && currentIdx < len(segs) {
		need := maxN - len(msgs)
		path := filepath.Join(b.cfg.Dir, segs[currentIdx])
		batch, endOffset, readErr := readFramesFrom(path, currentOffset, sig, need)

		if os.IsNotExist(readErr) {
			currentIdx++
			if currentIdx < len(segs) {
				currentFile = segs[currentIdx]
				currentOffset = 0
			}
			continue
		}
		if errors.Is(readErr, ErrCorruptSegment) {
			// Corrupt frame length — can't recover position in this file; skip to next.
			b.logger.Warn("telemetry buffer: corrupt frame in segment, skipping",
				zap.String("file", segs[currentIdx]))
			currentIdx++
			if currentIdx < len(segs) {
				currentFile = segs[currentIdx]
				currentOffset = 0
			}
			continue
		}
		if readErr != nil {
			return msgs, flushCursor{File: currentFile, Offset: currentOffset}, readErr
		}

		msgs = append(msgs, batch...)
		currentFile = segs[currentIdx]
		currentOffset = endOffset

		if len(batch) < need {
			// File exhausted — advance to next segment.
			currentIdx++
			if currentIdx < len(segs) {
				currentFile = segs[currentIdx]
				currentOffset = 0
			}
		} else {
			break
		}
	}

	return msgs, flushCursor{File: currentFile, Offset: currentOffset}, nil
}
