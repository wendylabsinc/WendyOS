package services

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// The audio adapter records at a fixed s16le / 48 kHz / mono format and writes
// self-contained WAV files, so fragments and segments are decodable without a
// sidecar index. This is the smallest honest implementation: it is levels
// driven (peak dBFS per chunk via computeAudioLevels) and seals raw PCM into
// WAV. A fuller version would carry the input's native rate and channel count,
// map hardware capture timestamps instead of the agent receipt, and support
// per-crossing pre-roll from the rolling buffer.
const (
	audioSampleRate     = 48000
	audioChannels       = 1
	audioBytesPerSample = 2
	audioBytesPerSecond = audioSampleRate * audioChannels * audioBytesPerSample
	// audioChunkBytes is the size of the read buffer, not the size of a read.
	// A pipe read returns whatever the capture process has produced: MEASURED
	// on a Jetson Orin Nano with a Logitech C920 microphone, reads arrive as
	// 8192 bytes (4096 frames, 85.33 ms), never the 9600 this bound would
	// suggest. Every duration derived from a chunk must therefore use the
	// length actually read.
	audioChunkBytes      = audioBytesPerSecond / 10
	audioSegmentDuration = 10 * time.Second
	audioSegmentBytes    = int64(audioBytesPerSecond) * int64(audioSegmentDuration/time.Second)
	// defaultAudioFragment is the sealed duration when a threshold source omits
	// an explicit fragment.
	defaultAudioFragment = 10 * time.Second
	// audioLevelField is the only field the audio threshold trigger understands.
	audioLevelField = "level_db"
	wavHeaderBytes  = 44
	// audioPipelineDelayEstimate is the capture delay still outstanding once
	// the chunk in hand has been accounted for: audio already sitting in the
	// device buffer when that chunk was handed over, plus its transit through
	// the capture subprocess pipe. MEASURED on a Jetson Orin Nano with a
	// Logitech C920 microphone over 60 reads, the ALSA device delay ran 12.6 ms
	// to 20.9 ms and pipe transit about 4 ms, giving 16.6 ms to 24.9 ms and
	// centring near 20 ms. The 102 ms by which the oldest sample in a chunk was
	// stale on that hardware is this residue plus the 85.33 ms chunk itself.
	audioPipelineDelayEstimate = 20 * time.Millisecond
	// audioPipelineDelayBound is the uncertainty half-width published alongside
	// a corrected stamp. It covers a residual delay anywhere between 0 and
	// 40 ms, which absorbs the period-size and scheduling variation a single
	// point measurement cannot pin down, and contains the arecord fallback path
	// now that audio.ArecordCommand bounds its device buffer to 40 ms.
	audioPipelineDelayBound = 20 * time.Millisecond
	// audioMappingSegment labels a stamp corrected by the two constants above,
	// so a reader can tell corrected audio stamps from the uncorrected
	// "receipt-bracket-v1" stamps the camera adapters still publish.
	audioMappingSegment = "receipt-minus-pipeline-v1"
)

// audioStream is the PCM source the capture loop reads from. It is an interface
// so tests can inject deterministic PCM without a live capture process.
type audioStream interface {
	Read([]byte) (int, error)
	Close()
	Err() string
}

// pcmCapture adapts the audio service's *capture process to audioStream.
type pcmCapture struct{ c *capture }

func (p *pcmCapture) Read(b []byte) (int, error) { return p.c.stdout.Read(b) }
func (p *pcmCapture) Close()                     { p.c.Close() }
func (p *pcmCapture) Err() string                { return p.c.Err() }

type audioDataAdapter struct {
	audio *AudioService
	// openStream is a seam over the real PCM capture; tests inject fake PCM.
	openStream func(ctx context.Context, deviceID uint32) (audioStream, error)
	// captureReceipt is a seam over data.CaptureReceipt, threaded into every
	// capture this adapter starts; tests script the clock so the stamping
	// arithmetic can be asserted exactly. Nil means read the real clock.
	captureReceipt func() (int64, int64, int64, error)
}

func newAudioDataAdapter(audioSvc *AudioService) dataCaptureAdapter {
	if audioSvc == nil {
		return nil
	}
	a := &audioDataAdapter{audio: audioSvc}
	a.openStream = a.defaultOpenStream
	return a
}

func (a *audioDataAdapter) defaultOpenStream(ctx context.Context, deviceID uint32) (audioStream, error) {
	target, err := captureTarget(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	rec, err := startCapture(ctx, target, audioSampleRate, audioChannels, "100ms")
	if err != nil {
		return nil, err
	}
	return &pcmCapture{c: rec}, nil
}

// audioHubDMAEndpointReason is appended to the description of an audio-hub DMA
// endpoint. It has to tell an operator both what is wrong and what would fix
// it, because the endpoint opens and streams perfectly: the only symptom is
// that every sample is zero.
const audioHubDMAEndpointReason = "audio hub DMA endpoint, not a physical input: captures digital silence unless an external I2S codec is wired and the audio hub is routed to it"

// admaifEndpointPattern matches the ALSA device identity that the Tegra Audio
// Processing Engine's ASoC driver gives an Audio DMA Interface (ADMAIF)
// front-end, as "arecord -l" renders it. On a Jetson Orin Nano that is
// "fe.admaif@290f000.ADMAIF1 (*) []" through to ADMAIF20.
//
// Matching a driver-formatted string is fragile, which is why it is isolated
// here behind isAudioHubDMAEndpoint: a future kernel may rename these, and this
// is the single place to fix. It is nevertheless the right signal, and the two
// seemingly more authoritative alternatives were measured on hardware and
// rejected:
//
//   - The "(*)" and "[]" in the name do not mean "unrouted". /proc/asound/
//     card2/pcm0c/info reports id "fe.admaif@290f000.ADMAIF1 (*)" with an empty
//     name field, and arecord prints "<id> [<name>]" — so "[]" is just the
//     empty PCM name and "(*)" is ASoC's marker for a dynamic PCM (a DPCM
//     front-end). Both are structural to an ADMAIF, present routed or not. Only
//     the "admaif" identity itself carries meaning.
//
//   - The audio hub crossbar mixer state is authoritative about routing, but
//     routing is not the question. On wendyos-hubert the "ADMAIF1 Mux" control
//     reads I2S2 and "ADMAIF2 Mux" reads I2S4 (NVIDIA's default board config),
//     yet a one-second capture from the routed plughw:2,0 still returns 192044
//     bytes containing zero non-zero samples, because /sys/kernel/debug/asoc/
//     components lists no codec beyond two snd-soc-dummy entries: the I2S ports
//     terminate in nothing. A mux-based health rule would therefore call
//     ADMAIF1 and ADMAIF2 healthy while they deliver pure silence, which is the
//     exact lie this check exists to remove — and it would cost twenty amixer
//     subprocesses per enumeration to get there.
//
// So health is decided on the structural question the identity does answer:
// this is a memory endpoint of the on-chip audio hub crossbar, not a physical
// capture input. The endpoint is still enumerated, because someone who wires an
// I2S codec and routes the hub has a legitimately usable ADMAIF and hiding it
// would blind us to real hardware. It is reported unhealthy rather than absent,
// and the reason names what to check.
var admaifEndpointPattern = regexp.MustCompile(`(?i)admaif@[0-9a-f]+\.admaif[0-9]+`)

// isAudioHubDMAEndpoint reports whether an audio source detail names an on-chip
// audio-hub DMA endpoint rather than a physical capture device.
func isAudioHubDMAEndpoint(detail string) bool {
	return admaifEndpointPattern.MatchString(detail)
}

func (a *audioDataAdapter) Discover(ctx context.Context) []data.Source {
	var nodes []audio.Node
	domain := "PIPEWIRE_CAPTURE/AGENT_RECEIPT"
	if audio.Available() {
		var err error
		nodes, _, err = audio.ListNodes(ctx)
		if err != nil {
			return nil
		}
	} else {
		var err error
		nodes, err = audio.ListAlsaNodes(ctx)
		if err != nil {
			return nil
		}
		domain = "ALSA_CAPTURE/AGENT_RECEIPT"
	}
	out := make([]data.Source, 0, len(nodes))
	for _, n := range nodes {
		if n.IsSink {
			continue
		}
		detail := strings.TrimSpace(n.Description + " " + n.Name)
		healthy := true
		if isAudioHubDMAEndpoint(detail) {
			healthy = false
			detail = strings.TrimSpace(detail + "; " + audioHubDMAEndpointReason)
		}
		out = append(out, data.Source{
			ID:          fmt.Sprintf("audio:%d", n.ID),
			Kind:        "audio",
			ClockDomain: domain,
			Healthy:     healthy,
			Detail:      detail,
		})
	}
	return out
}

func audioDeviceID(sourceID string) (uint32, bool) {
	if !strings.HasPrefix(sourceID, "audio:") {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(sourceID, "audio:"), 10, 32)
	return uint32(n), err == nil
}

func (a *audioDataAdapter) Start(ctx context.Context, session data.CaptureSession, selected []data.Source) (runningDataCapture, error) {
	group := &audioCaptureGroup{}
	for _, source := range selected {
		devID, ok := audioDeviceID(source.ID)
		if !ok {
			continue
		}
		capture, err := a.startOne(ctx, session, source, devID)
		if err != nil {
			_, _ = group.Stop(context.Background())
			return nil, fmt.Errorf("%s: %w", source.ID, err)
		}
		group.captures = append(group.captures, capture)
	}
	if len(group.captures) == 0 {
		return nil, nil
	}
	return group, nil
}

func (a *audioDataAdapter) startOne(ctx context.Context, session data.CaptureSession, source data.Source, devID uint32) (*audioCapture, error) {
	mode := "continuous"
	var notes []string
	var op string
	var threshold float64
	var fragmentBytes int64

	capture := source.Capture
	if capture.EffectiveMode() == "threshold" {
		field, operator, value, err := data.ParseFieldThreshold(capture.Trigger)
		if err != nil {
			return nil, err
		}
		if field != audioLevelField {
			notes = append(notes, fmt.Sprintf("audio threshold triggers only on %s, not %q; recording continuously", audioLevelField, field))
		} else {
			mode = "threshold"
			op, threshold = operator, value
			fragment := defaultAudioFragment
			if d := capture.Fragment; d != "" {
				if parsed, perr := time.ParseDuration(d); perr == nil && parsed > 0 {
					fragment = parsed
				} else {
					notes = append(notes, fmt.Sprintf("fragment %q is not a positive duration; sealing %s per crossing", d, defaultAudioFragment))
				}
			} else {
				notes = append(notes, fmt.Sprintf("threshold source declared no fragment; sealing %s per crossing", defaultAudioFragment))
			}
			fragmentBytes = int64(fragment.Seconds() * float64(audioBytesPerSecond))
			notes = append(notes, fmt.Sprintf("threshold capture: sealing a %s fragment when %s %s %.3g dBFS", fragment, audioLevelField, op, threshold))
		}
	}

	stream, err := a.openStream(ctx, devID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(session.Directory, "audio", safeCaptureName(source.ID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		stream.Close()
		return nil, err
	}
	index, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		stream.Close()
		return nil, err
	}

	captureCtx, cancel := context.WithCancel(context.Background())
	c := &audioCapture{
		source: source, session: session, dir: dir, stream: stream, index: index,
		ctx: captureCtx, cancel: cancel, done: make(chan struct{}), ready: make(chan error, 1),
		mode: mode, op: op, threshold: threshold, fragmentBytes: fragmentBytes, notes: notes,
		captureReceipt: a.captureReceipt,
	}
	go c.run()
	select {
	case err := <-c.ready:
		if err != nil {
			c.shutdown()
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		c.shutdown()
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		c.shutdown()
		return nil, errors.New("timed out waiting for first audio chunk")
	}
}

// shutdown cancels the capture context and closes the PCM stream, unblocking a
// pending Read, then waits for run to finish.
func (c *audioCapture) shutdown() {
	c.cancel()
	c.stream.Close()
	<-c.done
}

type audioCaptureGroup struct{ captures []*audioCapture }

func (g *audioCaptureGroup) Stop(ctx context.Context) ([]data.CaptureResult, error) {
	var results []data.CaptureResult
	var errs []error
	for i := len(g.captures) - 1; i >= 0; i-- {
		r, err := g.captures[i].Stop(ctx)
		results = append(results, r...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return results, errors.Join(errs...)
}

type audioCapture struct {
	source  data.Source
	session data.CaptureSession
	dir     string
	stream  audioStream
	index   *os.File

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan error
	readyOnce sync.Once
	stopOnce  sync.Once
	result    data.CaptureResult
	runErr    error

	// mode is "continuous" or "threshold".
	mode string
	// Threshold configuration.
	op            string
	threshold     float64
	fragmentBytes int64
	// Threshold state.
	wasAbove bool
	sealing  bool

	// Current segment (WAV) state, shared by both modes.
	seg          *os.File
	segRel       string
	segBytes     int64
	segNumber    int
	segCanonical int64
	segUncert    int64
	segReceipt   int64
	segTrigger   *float64
	// segClamped and segClampShortfall carry the origin clamp (see
	// beginSegment) from the stamp to the segment's index record.
	segClamped        bool
	segClampShortfall int64

	// Accounting.
	missedCrossings uint64
	// clampedSegments counts segment stamps held at the episode origin because
	// the pipeline correction would have placed them before it.
	clampedSegments   uint64
	maxCanonicalError int64
	firstOffset       *int64
	notes             []string

	// captureReceipt is a test seam over data.CaptureReceipt.
	captureReceipt func() (int64, int64, int64, error)
}

func (c *audioCapture) receiptNow() (int64, int64, int64, error) {
	if c.captureReceipt != nil {
		return c.captureReceipt()
	}
	return data.CaptureReceipt()
}

func (c *audioCapture) signalReady(err error) { c.readyOnce.Do(func() { c.ready <- err }) }

// run reads PCM until the stream ends. It does not poll c.ctx: Stop closes the
// stream, which unblocks Read and drains the buffered PCM, so a stop never
// truncates data the producer already delivered.
func (c *audioCapture) run() {
	defer close(c.done)
	defer c.finish()
	buf := make([]byte, audioChunkBytes)
	for {
		n, err := c.stream.Read(buf)
		if n > 0 {
			if perr := c.handleChunk(buf[:n]); perr != nil {
				c.runErr = perr
				c.signalReady(perr)
				return
			}
			c.signalReady(nil)
		}
		if err != nil {
			if err != io.EOF {
				if msg := c.stream.Err(); msg != "" {
					c.runErr = errors.New(msg)
				} else {
					c.runErr = err
				}
			}
			c.signalReady(c.runErr)
			return
		}
	}
}

func (c *audioCapture) handleChunk(pcm []byte) error {
	if c.mode == "threshold" {
		return c.handleThresholdChunk(pcm)
	}
	return c.handleContinuousChunk(pcm)
}

// handleThresholdChunk detects a rising crossing of the level threshold and
// seals a fragment of the configured duration. A crossing that occurs while a
// fragment is still being sealed is counted as a missed crossing (an honest
// drop) rather than starting an overlapping fragment.
func (c *audioCapture) handleThresholdChunk(pcm []byte) error {
	peak, _ := computeAudioLevels(pcm)
	aboveNow := data.CompareThreshold(float64(peak), c.op, c.threshold)
	crossing := aboveNow && !c.wasAbove
	c.wasAbove = aboveNow

	if crossing {
		if c.sealing {
			c.missedCrossings++
		} else {
			level := float64(peak)
			if err := c.beginSegment(&level, len(pcm)); err != nil {
				return err
			}
			c.sealing = true
		}
	}
	if !c.sealing {
		return nil
	}
	remaining := c.fragmentBytes - c.segBytes
	if remaining <= 0 {
		if err := c.finishSegment(false); err != nil {
			return err
		}
		c.result.Count++
		c.sealing = false
		return nil
	}
	write := pcm
	if int64(len(write)) > remaining {
		write = write[:remaining]
	}
	if err := c.writeSegment(write); err != nil {
		return err
	}
	if c.segBytes >= c.fragmentBytes {
		if err := c.finishSegment(false); err != nil {
			return err
		}
		c.result.Count++
		c.sealing = false
	}
	return nil
}

// handleContinuousChunk appends PCM to a rotating WAV segment.
func (c *audioCapture) handleContinuousChunk(pcm []byte) error {
	if c.seg == nil {
		if err := c.beginSegment(nil, len(pcm)); err != nil {
			return err
		}
	}
	if err := c.writeSegment(pcm); err != nil {
		return err
	}
	if c.segBytes >= audioSegmentBytes {
		if err := c.finishSegment(false); err != nil {
			return err
		}
		c.result.Count++
	}
	return nil
}

// beginSegment stamps the segment start on the canonical episode timeline and
// opens a WAV file with a placeholder header (patched on finish).
//
// firstChunkBytes is the length of the chunk that opened the segment, which has
// already been read from the capture pipe by the time the clock is read. The
// segment stamp labels that chunk's FIRST sample, so the receipt has to be
// walked back past the whole chunk and past the delay still outstanding behind
// it. Passing the length rather than assuming audioChunkBytes matters: a pipe
// read routinely returns less than the buffer holds.
func (c *audioCapture) beginSegment(triggerLevel *float64, firstChunkBytes int) error {
	canonical, uncertainty, receipt, err := c.canonicalTime()
	if err != nil {
		return err
	}
	canonical -= pcmDurationNanos(firstChunkBytes) + int64(audioPipelineDelayEstimate)
	uncertainty += int64(audioPipelineDelayBound)
	// The correction is an estimate walking backwards, and nothing stops it
	// walking back past the start of the episode. Audio has no pre-roll ring:
	// capture begins when the episode does, so there are no samples from before
	// the origin and a stamp before it would advertise pre-trigger audio that
	// does not exist. A negative offset is how this tree says "pre-roll was
	// achieved", and the audio adapter is never entitled to say it.
	//
	// So the stamp is held at the origin — and said so, rather than quietly
	// maxed. The shortfall widens the published uncertainty, which keeps the
	// true instant inside the bracket, and the segment's index record carries
	// the flag and the amount so a reader can tell a bound from an estimate.
	segmentClamped, clampShortfall := false, int64(0)
	if shortfall := c.session.RequestBootNanos - canonical; shortfall > 0 {
		canonical = c.session.RequestBootNanos
		uncertainty += shortfall
		segmentClamped, clampShortfall = true, shortfall
		c.clampedSegments++
	}
	if uncertainty > c.maxCanonicalError {
		c.maxCanonicalError = uncertainty
	}
	c.segNumber++
	prefix := "segment"
	if c.mode == "threshold" {
		prefix = "fragment"
	}
	name := fmt.Sprintf("%s-%06d.wav", prefix, c.segNumber)
	f, err := os.OpenFile(filepath.Join(c.dir, name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	if err := writeWAVHeader(f, audioSampleRate, audioChannels, 0); err != nil {
		f.Close()
		return err
	}
	c.seg = f
	c.segRel = filepath.ToSlash(filepath.Join("audio", safeCaptureName(c.source.ID), name))
	c.segBytes = 0
	c.segCanonical = canonical
	c.segUncert = uncertainty
	c.segReceipt = receipt
	c.segTrigger = triggerLevel
	c.segClamped = segmentClamped
	c.segClampShortfall = clampShortfall
	if c.firstOffset == nil {
		offset := canonical - c.session.RequestBootNanos
		c.firstOffset = &offset
	}
	return nil
}

func (c *audioCapture) writeSegment(pcm []byte) error {
	n, err := c.seg.Write(pcm)
	if err != nil {
		return err
	}
	if n != len(pcm) {
		return ioErrShortWrite(n, len(pcm))
	}
	c.segBytes += int64(n)
	return nil
}

// finishSegment patches the WAV header with the real data size, closes the
// file, and records the segment in index.jsonl.
func (c *audioCapture) finishSegment(truncated bool) error {
	if c.seg == nil {
		return nil
	}
	dataBytes := c.segBytes
	var writeErr error
	if _, err := c.seg.Seek(0, io.SeekStart); err != nil {
		writeErr = err
	} else if err := writeWAVHeader(c.seg, audioSampleRate, audioChannels, uint32(dataBytes)); err != nil {
		writeErr = err
	}
	if syncErr := c.seg.Sync(); writeErr == nil {
		writeErr = syncErr
	}
	if closeErr := c.seg.Close(); writeErr == nil {
		writeErr = closeErr
	}
	c.seg = nil
	if writeErr != nil {
		return writeErr
	}
	record := audioIndexRecord{
		CanonicalEpisodeNanos:     c.segCanonical - c.session.RequestBootNanos,
		CanonicalUncertaintyNanos: c.segUncert,
		AgentReceiptBootNanos:     c.segReceipt,
		MappingSegment:            audioMappingSegment,
		Segment:                   c.segRel,
		ByteSize:                  dataBytes,
		SampleRate:                audioSampleRate,
		Channels:                  audioChannels,
		DurationNanos:             pcmDurationNanos(int(dataBytes)),
		Mode:                      c.mode,
		TriggerLevelDb:            c.segTrigger,
		Truncated:                 truncated,
		ClampedToOrigin:           c.segClamped,
		ClampShortfallNanos:       c.segClampShortfall,
	}
	b, _ := json.Marshal(record)
	_, err := c.index.Write(append(b, '\n'))
	c.segBytes = 0
	return err
}

// canonicalTime reads the agent receipt and the half-width of the clock bracket
// it was taken in. Both are reported raw: the bracket measures how long the two
// CLOCK_BOOTTIME reads took and nothing else, and it is beginSegment that folds
// in the capture delay the receipt knows nothing about.
func (c *audioCapture) canonicalTime() (canonical, uncertainty, receipt int64, err error) {
	before, mid, after, err := c.receiptNow()
	if err != nil {
		return 0, 0, 0, err
	}
	canonical = mid
	uncertainty = (after - before + 1) / 2
	return canonical, uncertainty, mid, nil
}

// pcmDurationNanos converts a count of captured bytes to the time they span at
// the fixed s16le / 48 kHz / mono capture format.
func pcmDurationNanos(n int) int64 {
	return int64(n) * int64(time.Second) / int64(audioBytesPerSecond)
}

// finish flushes any in-progress segment and folds accounting into the result.
func (c *audioCapture) finish() {
	if c.seg != nil {
		// A fragment cut short by capture stop is still a valid, shorter WAV.
		truncated := c.mode == "threshold" && c.sealing
		if truncated {
			c.notes = append(c.notes, "final fragment truncated by capture stop")
		}
		if err := c.finishSegment(truncated); err == nil {
			c.result.Count++
		}
	}
	c.result.SourceID = c.source.ID
	c.result.ClockDomain = c.source.ClockDomain
	c.result.ActualOffset = c.firstOffset
	if c.clampedSegments > 0 {
		// Said in the manifest as well as per segment: an operator reading the
		// episode summary should not have to open the audio index to learn that
		// some stamps are bounds held at the origin rather than estimates.
		c.notes = append(c.notes, fmt.Sprintf(
			"%d segment stamp(s) clamped to the episode origin: the pipeline correction reached before it, and their canonical uncertainty is widened by the shortfall",
			c.clampedSegments))
	}
	if c.maxCanonicalError > 0 {
		maxError := c.maxCanonicalError
		c.result.MappingError = &maxError
	}
	if c.mode == "threshold" {
		missed := c.missedCrossings
		c.result.Drops = &missed
		c.result.DropAccounting = "missed_threshold_crossings_during_seal"
	} else {
		c.result.DropAccounting = "pcm_stream_has_no_sequence_numbers"
	}
	if len(c.notes) > 0 {
		c.result.SourceDetail = strings.TrimSpace(c.source.Detail + " (" + strings.Join(c.notes, "; ") + ")")
	}
	_ = c.index.Sync()
	_ = c.index.Close()
}

func (c *audioCapture) Stop(context.Context) ([]data.CaptureResult, error) {
	c.stopOnce.Do(c.shutdown)
	return []data.CaptureResult{c.result}, c.runErr
}

type audioIndexRecord struct {
	CanonicalEpisodeNanos     int64    `json:"canonical_episode_nanos"`
	CanonicalUncertaintyNanos int64    `json:"canonical_uncertainty_nanos"`
	AgentReceiptBootNanos     int64    `json:"agent_receipt_boottime_nanos"`
	MappingSegment            string   `json:"mapping_segment"`
	Segment                   string   `json:"segment"`
	ByteSize                  int64    `json:"byte_size"`
	SampleRate                uint32   `json:"sample_rate"`
	Channels                  uint32   `json:"channels"`
	DurationNanos             int64    `json:"duration_nanos"`
	Mode                      string   `json:"mode"`
	TriggerLevelDb            *float64 `json:"trigger_level_db,omitempty"`
	Truncated                 bool     `json:"truncated,omitempty"`
	// ClampedToOrigin marks a stamp whose pipeline correction reached back past
	// the episode origin and was held at it. Audio has no pre-roll ring, so it
	// cannot have captured anything before the origin; the correction is an
	// estimate and a segment opened within roughly a chunk of the origin can
	// therefore be walked back further than the capture actually goes.
	// CanonicalUncertaintyNanos is widened by the shortfall on such a record,
	// so the true instant still lies inside the published bracket, and this
	// flag says the stamp is a bound rather than an estimate.
	ClampedToOrigin bool `json:"clamped_to_origin,omitempty"`
	// ClampShortfallNanos is how far past the episode origin the correction
	// would have reached. Present only alongside ClampedToOrigin.
	ClampShortfallNanos int64 `json:"clamp_shortfall_nanos,omitempty"`
}

// writeWAVHeader writes a 44-byte canonical PCM WAV header at the file cursor.
// dataBytes is the size of the PCM payload that follows; it is zero for the
// placeholder written before recording and the real size when patched on close.
func writeWAVHeader(f *os.File, sampleRate, channels, dataBytes uint32) error {
	var buf [wavHeaderBytes]byte
	byteRate := sampleRate * channels * audioBytesPerSample
	blockAlign := uint16(channels * audioBytesPerSample)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 36+dataBytes)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], 16) // bits per sample
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataBytes)
	_, err := f.Write(buf[:])
	return err
}
