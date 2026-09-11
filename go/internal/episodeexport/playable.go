// Package episodeexport turns an episode's camera capture into files that
// ordinary video players can play, without touching the episode itself.
//
// The problem it solves. Episode camera capture keeps an H.264 Annex-B
// elementary stream (cameras/<source>/segment-NNNNNN.h264). An elementary
// stream is a bare sequence of coded pictures: it has no container, and
// therefore carries no timing whatsoever. A player handed one has nothing to
// derive a presentation schedule from, so it invents a frame rate, and the clip
// runs at the wrong speed and reports the wrong duration.
//
// Why a fixed frame rate is not the fix. Passing a constant rate, whether
// guessed or measured as an average, replaces one wrong schedule with another.
// The capture rate is genuinely variable: it falls as the device gets busy with
// inference, so no single number describes more than a fraction of the clip.
// One measured episode held 176 frames across 9.95 seconds, an average near
// 17.7 Hz, but the instantaneous interval wandered well away from that and the
// next episode has no reason to average the same.
//
// Where the real timing comes from. It was recorded all along.
// cameras/<source>/index.jsonl holds one line per kept frame carrying
// canonical_episode_nanos, the frame's position on the episode's canonical
// CLOCK_BOOTTIME timeline, along with the segment file, byte offset and byte
// size of its payload. This package remuxes those same payload bytes into an
// MP4 whose sample table gives every frame the presentation time the index
// recorded for it. Nothing here computes, averages, or assumes a rate.
//
// It is a remux, not a transcode: the coded pictures are copied verbatim and
// only re-framed, so the output is bit-for-bit the same video.
//
// Container choice. MP4 with an explicit per-sample duration for every sample
// in the stts box. Matroska is the more usual home for variable frame rate and
// mkvmerge --timestamps the usual route to it, but mkvmerge is an extra
// dependency and QuickTime Player cannot open Matroska at all, whereas MP4
// represents variable frame rate exactly as precisely and plays unmodified in
// both VLC and QuickTime. The muxer below is pure Go, so the tool has no
// external dependency to be missing: there is no ffmpeg, mkvmerge or PyAV in
// the path from an episode to a playable file.
//
// The episode is read-only here. Convert writes into a separate destination
// directory. The elementary stream and index are the checksummed archival
// truth, and index.jsonl addresses frames by byte offset within the stream, so
// rewriting it in place would break the join between a frame and the model
// input recorded against it.
//
// ConvertSourceInPlace is the one sanctioned exception to that separation: the
// agent calls it while sealing an episode, before the manifest's file list is
// finalized, to add cameras/<source>/playable.mp4 beside the raw capture. The
// raw segments and index are still only read; the derived clip is a new file
// that the seal then checksums, lists and uploads like everything else, so a
// browser can play the episode straight from the bucket.
package episodeexport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// playableTimescale is the MP4 media timescale, in ticks per second. One tick
// is a microsecond: fine enough that rounding a recorded nanosecond timestamp
// is invisible beside any real frame interval, and coarse enough that a
// per-sample duration stays inside the 32-bit stts field for any gap short of
// 71 minutes.
const playableTimescale = 1_000_000

// PlayableFileName is the derived MP4 written into a camera source directory
// at seal time by ConvertSourceInPlace. The agent's manifest code and the
// episode-playable command both key on this name.
const PlayableFileName = "playable.mp4"

// ClipResult reports what one camera source produced, in the numbers needed to
// check the conversion without watching it.
type ClipResult struct {
	// Source is the encoded camera source directory name.
	Source string
	// Output is the playable file written, empty when nothing was playable.
	Output string
	// IndexLines counts non-empty lines in index.jsonl, the number of frames
	// the episode claims to have kept.
	IndexLines int
	// Frames counts samples actually written to the output.
	Frames int
	// Skipped counts frames the index named but which could not be muxed
	// because their bytes were missing, truncated, or held no coded picture.
	Skipped int
	// SkippedReason quotes the first failure behind Skipped, so a clip that
	// lost frames for one systemic reason (a segment file that is not there,
	// an index whose offsets run past the end of a truncated segment) names
	// that reason instead of reporting a bare count. Empty when nothing was
	// skipped, or when the frames skipped carried no coded picture at all and
	// so produced no read error.
	SkippedReason string
	// Unparsed counts index lines that were not valid records, which is the
	// expected shape of a partial tail left by an interrupted episode.
	Unparsed int
	// Segments counts distinct segment files the frames came from.
	Segments int
	// Span is the index's last-minus-first canonical timestamp.
	Span time.Duration
	// BFrames records that the stream contains B slices, so its presentation
	// order is not the coded order the index records and the timing written
	// here cannot be exact. See isBSlice.
	BFrames bool
	// UndecodedSliceHeaders counts slice headers whose type could not be read,
	// which is the honest "unknown" beside BFrames: a header this tool cannot
	// parse is not evidence that the stream is free of B slices, so a clip with
	// BFrames false and this non-zero has undetermined timing exactness rather
	// than proven exactness.
	UndecodedSliceHeaders int
	// SyncSamples counts written samples a decoder can start on (coded IDR
	// pictures). A clip with none is not seekable and most players refuse it,
	// so it must not be reported as a clean conversion on frame count alone.
	SyncSamples int
	// ParameterSetChanges counts in-band SPS or PPS units whose bytes differ
	// from the first ones seen. Encoders repeat identical parameter sets
	// before every IDR and those repeats are not counted; a change means the
	// producer restarted mid-episode with new parameters (a different
	// resolution, most likely). The MP4 written here carries exactly one
	// decoder configuration, built from the first SPS/PPS, so every frame
	// after a change would be decoded against the wrong parameters. A clip
	// with this non-zero must not be presented as a faithful rendering of the
	// capture.
	ParameterSetChanges int
	// MinInterval, MaxInterval and MeanInterval describe the spread of
	// inter-frame intervals actually written. They differ from each other
	// whenever the capture rate varied, which is the point.
	MinInterval, MaxInterval, MeanInterval time.Duration
}

// Convert writes one playable MP4 per camera source found under
// episodeDir/cameras into outDir, named <source>.mp4. The episode directory is
// only ever read. A source whose clip cannot be built is reported in the
// returned error list and skipped, so one damaged camera does not deny the
// others.
func Convert(episodeDir, outDir string) ([]ClipResult, []error) {
	indexes, err := filepath.Glob(filepath.Join(episodeDir, "cameras", "*", "index.jsonl"))
	if err != nil {
		return nil, []error{fmt.Errorf("scanning camera sources: %w", err)}
	}
	if len(indexes) == 0 {
		return nil, []error{fmt.Errorf("no cameras/<source>/index.jsonl under %s: not an episode directory, or it recorded no camera", episodeDir)}
	}
	sort.Strings(indexes)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, []error{err}
	}

	var (
		results []ClipResult
		errs    []error
	)
	for _, index := range indexes {
		sourceDir := filepath.Dir(index)
		source := filepath.Base(sourceDir)
		out := filepath.Join(outDir, source+".mp4")
		result, err := convertSource(episodeDir, sourceDir, index, out)
		result.Source = source
		if err != nil {
			errs = append(errs, fmt.Errorf("camera %s: %w", source, err))
			results = append(results, result)
			continue
		}
		results = append(results, result)
	}
	return results, errs
}

// ConvertSourceInPlace remuxes one camera source into
// <sourceDir>/playable.mp4, reading timing from <sourceDir>/index.jsonl. It is
// the seal-time variant of Convert: the caller is the agent finalizing an
// episode, and the output lands inside the episode so the manifest can list
// it. The raw segment files and the index are only ever read. The output is
// written through a temporary file and renamed, so a crash mid-mux leaves no
// half-written playable.mp4 behind (sealing ignores *.tmp files).
//
// A non-nil error means no playable.mp4 was left in place. A nil error means
// the file exists, but the caller must still judge the ClipResult before
// trusting it: BFrames, UndecodedSliceHeaders and SyncSamples report
// conditions under which the clip's timing or seekability cannot be vouched
// for, and Convert deliberately does not decide policy for its callers.
func ConvertSourceInPlace(episodeDir, sourceDir string) (ClipResult, error) {
	result, err := convertSource(episodeDir, sourceDir, filepath.Join(sourceDir, "index.jsonl"), filepath.Join(sourceDir, PlayableFileName))
	result.Source = filepath.Base(sourceDir)
	return result, err
}

func convertSource(episodeDir, sourceDir, indexPath, out string) (ClipResult, error) {
	frames, result, err := readCameraIndex(indexPath)
	if err != nil {
		return result, err
	}
	if len(frames) == 0 {
		return result, fmt.Errorf("index names no usable H.264 frames (%d unparsed lines)", result.Unparsed)
	}
	result.Span = time.Duration(frames[len(frames)-1].CanonicalEpisodeNanos - frames[0].CanonicalEpisodeNanos)

	tmp := out + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return result, err
	}
	defer os.Remove(tmp)
	stats, writeErr := muxAnnexBToMP4(f, episodeDir, frames)
	closeErr := f.Close()
	result.Frames, result.Skipped, result.Segments = stats.frames, stats.skipped, stats.segments
	result.SkippedReason = stats.skipReason
	result.BFrames = stats.bFrames
	result.UndecodedSliceHeaders = stats.undecodedSliceHeaders
	result.SyncSamples = stats.syncSamples
	result.ParameterSetChanges = stats.parameterSetChanges
	result.MinInterval, result.MaxInterval, result.MeanInterval = stats.min, stats.max, stats.mean
	if err := errors.Join(writeErr, closeErr); err != nil {
		return result, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return result, err
	}
	result.Output = out
	return result, nil
}

// cameraIndexLine is the read-side view of one cameras/<source>/index.jsonl
// record, naming only the fields the remux needs. The writer's full record
// lives with the camera adapter in the agent.
type cameraIndexLine struct {
	CanonicalEpisodeNanos int64  `json:"canonical_episode_nanos"`
	Segment               string `json:"segment"`
	ByteOffset            int64  `json:"byte_offset"`
	ByteSize              int    `json:"byte_size"`
	Codec                 string `json:"codec"`
}

// readCameraIndex reads the frame index and returns its H.264 frames sorted by
// canonical episode time. Lines that are not valid records, which is what the
// tail of an interrupted episode's index looks like, are counted and skipped
// rather than guessed at.
func readCameraIndex(path string) ([]cameraIndexLine, ClipResult, error) {
	var result ClipResult
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, result, err
	}
	var frames []cameraIndexLine
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		result.IndexLines++
		var rec cameraIndexLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			result.Unparsed++
			continue
		}
		if rec.Segment == "" || rec.ByteSize <= 0 || !strings.HasSuffix(strings.ToLower(rec.Segment), ".h264") {
			result.Unparsed++
			continue
		}
		frames = append(frames, rec)
	}
	sort.SliceStable(frames, func(i, j int) bool {
		return frames[i].CanonicalEpisodeNanos < frames[j].CanonicalEpisodeNanos
	})
	return frames, result, nil
}

// sample is one muxed access unit: its presentation time in media ticks, where
// its bytes landed in mdat, and whether a decoder can start on it.
type sample struct {
	timestamp int64
	offset    int64
	size      int64
	sync      bool
}

type muxStats struct {
	frames, skipped, segments int
	min, max, mean            time.Duration
	bFrames                   bool
	// skipReason is the first error behind skipped, kept so a clip that lost
	// frames for one systemic reason can name it instead of reporting a count
	// whose cause was thrown away.
	skipReason string
	// undecodedSliceHeaders counts slice headers whose type could not be read.
	// isBSlice cannot answer for those, and "could not tell" is not "no B
	// slices", so they are counted rather than folded into bFrames.
	undecodedSliceHeaders int
	// syncSamples counts written samples a decoder can start on.
	syncSamples int
	// parameterSetChanges counts SPS/PPS units that differ byte-wise from the
	// first ones seen; identical repeats before each IDR are not counted.
	parameterSetChanges int
}

// isBSlice reports whether a VCL NAL unit carries a B slice. It matters
// because the index records frames in capture order, which is coded order, and
// this remux presents them in that same order. That is exact for the
// B-frame-free streams the device's own encoder produces, but a stream with B
// slices has a presentation order that differs from its coded order, and
// recovering it needs picture order counts the index does not carry. Such a
// stream is flagged rather than silently mistimed.
//
// It returns (isB, parsed); parsed is false when the slice header could not be
// read, which is an honest "unknown" rather than a "no". Reporting an
// unparsable header as B-free would let a clip whose timing this tool cannot
// vouch for be presented as exactly timed.
func isBSlice(nal []byte) (isB, parsed bool) {
	if len(nal) < 2 {
		return false, false
	}
	r := &bitReader{b: unescapeRBSP(nal)}
	r.skip(8) // nal_unit_header
	r.ue()    // first_mb_in_slice
	sliceType := r.ue()
	if r.err != nil {
		return false, false
	}
	return sliceType == 1 || sliceType == 6, true
}

// noteSliceType folds one slice header's verdict into the stats.
func (s *muxStats) noteSliceType(nal []byte) {
	isB, parsed := isBSlice(nal)
	if !parsed {
		s.undecodedSliceHeaders++
		return
	}
	s.bFrames = s.bFrames || isB
}

// muxAnnexBToMP4 writes a complete MP4 to w. Sample payloads are streamed
// straight from the segment files into mdat, so peak memory does not grow with
// the length of the episode; the sample table is then written from the offsets
// and sizes that streaming recorded.
func muxAnnexBToMP4(w *os.File, episodeDir string, frames []cameraIndexLine) (muxStats, error) {
	var stats muxStats
	segments := &segmentReader{root: episodeDir, seen: map[string]bool{}}
	defer segments.Close()

	ftyp := box("ftyp", concat(
		[]byte("isom"), u32(0x200),
		[]byte("isom"), []byte("iso2"), []byte("avc1"), []byte("mp41"),
	))
	if _, err := w.Write(ftyp); err != nil {
		return stats, err
	}
	// A 64-bit mdat header is written unconditionally so its payload size,
	// known only once every sample has been copied, can be patched in place
	// without moving any sample bytes.
	const mdatHeader = int64(16)
	mdatStart := int64(len(ftyp))
	if _, err := w.Write(concat(u32(1), []byte("mdat"), u64(0))); err != nil {
		return stats, err
	}

	var (
		samples  []sample
		sps, pps []byte
		cursor   = mdatStart + mdatHeader
	)
	base := frames[0].CanonicalEpisodeNanos
	for _, frame := range frames {
		payload, err := segments.read(frame)
		if err != nil {
			// A frame the index names but whose bytes are missing or short is
			// dropped and counted. Writing the bytes that do exist would put a
			// truncated access unit in the file and make it undecodable from
			// that point on. The first cause is kept: a bare count cannot tell
			// a missing segment file from an index that outran a truncated one.
			stats.skipped++
			if stats.skipReason == "" {
				stats.skipReason = err.Error()
			}
			continue
		}
		var (
			body []byte
			sync bool
		)
		for _, nal := range splitAnnexB(payload) {
			if len(nal) == 0 {
				continue
			}
			switch nal[0] & 0x1f {
			case 7: // sequence parameter set, carried in avcC instead
				if sps == nil {
					sps = append([]byte(nil), nal...)
				} else if !bytes.Equal(sps, nal) {
					// The producer restarted with new parameters. avcC holds
					// only the first SPS, so frames after this point would be
					// decoded against the wrong configuration; count it so
					// callers can refuse the clip instead of shipping one that
					// silently misdecodes its tail.
					stats.parameterSetChanges++
				}
				continue
			case 8: // picture parameter set, carried in avcC instead
				if pps == nil {
					pps = append([]byte(nil), nal...)
				} else if !bytes.Equal(pps, nal) {
					stats.parameterSetChanges++
				}
				continue
			case 9, 12: // access unit delimiter, filler
				continue
			case 5: // coded slice of an IDR picture
				sync = true
				stats.noteSliceType(nal)
			case 1: // coded slice of a non-IDR picture
				stats.noteSliceType(nal)
			}
			body = append(body, u32(uint32(len(nal)))...)
			body = append(body, nal...)
		}
		if len(body) == 0 {
			stats.skipped++
			continue
		}
		if _, err := w.Write(body); err != nil {
			return stats, err
		}
		if sync {
			stats.syncSamples++
		}
		samples = append(samples, sample{
			// Rounded from the recorded nanoseconds. Never derived from a rate.
			timestamp: (frame.CanonicalEpisodeNanos - base + 500) / 1000,
			offset:    cursor,
			size:      int64(len(body)),
			sync:      sync,
		})
		cursor += int64(len(body))
	}
	stats.frames, stats.segments = len(samples), len(segments.seen)
	if len(samples) == 0 {
		if stats.skipReason != "" {
			return stats, fmt.Errorf("no frame payload could be read (%d unreadable); first cause: %s", stats.skipped, stats.skipReason)
		}
		return stats, fmt.Errorf("no frame payload could be read (%d unreadable)", stats.skipped)
	}
	if sps == nil || pps == nil {
		return stats, errors.New("stream carries no SPS/PPS, so no decoder configuration can be written")
	}
	width, height, err := spsDimensions(sps)
	if err != nil {
		return stats, fmt.Errorf("reading picture size from SPS: %w", err)
	}

	if _, err := w.WriteAt(u64(uint64(cursor-mdatStart)), mdatStart+8); err != nil {
		return stats, err
	}
	if _, err := w.Seek(cursor, io.SeekStart); err != nil {
		return stats, err
	}
	moov, durations, err := buildMoov(samples, sps, pps, width, height)
	if err != nil {
		return stats, err
	}
	if _, err := w.Write(moov); err != nil {
		return stats, err
	}
	stats.min, stats.max, stats.mean = intervalSpread(durations)
	return stats, nil
}

// intervalSpread summarises the inter-frame intervals written, over the
// intervals between consecutive frames only: the final sample's hold time is
// not a measured interval and is excluded.
func intervalSpread(durations []uint32) (min, max, mean time.Duration) {
	if len(durations) < 2 {
		return 0, 0, 0
	}
	measured := durations[:len(durations)-1]
	lo, hi, total := measured[0], measured[0], uint64(0)
	for _, d := range measured {
		if d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
		total += uint64(d)
	}
	tick := time.Second / playableTimescale
	return time.Duration(lo) * tick,
		time.Duration(hi) * tick,
		time.Duration(total/uint64(len(measured))) * tick
}

// segmentReader reads frame payloads out of the episode's segment files,
// keeping the current segment open across the run of frames that share it. The
// index's segment paths are episode-relative, so several segments per source
// and several sources per episode both fall out naturally.
type segmentReader struct {
	root string
	name string
	file *os.File
	size int64
	seen map[string]bool
}

func (s *segmentReader) read(frame cameraIndexLine) ([]byte, error) {
	if s.name != frame.Segment {
		s.close()
		f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(frame.Segment)))
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		s.name, s.file, s.size = frame.Segment, f, info.Size()
		s.seen[frame.Segment] = true
	}
	// byte_size from the index is authoritative for how many bytes belong to
	// this frame. The model-input ledger's payload_bytes can exceed it for a
	// frame that opens a segment, because the segment begins at the
	// parameter-set prefix inside that payload rather than at its first byte.
	if frame.ByteOffset < 0 || frame.ByteOffset+int64(frame.ByteSize) > s.size {
		return nil, fmt.Errorf("frame at offset %d size %d exceeds %s (%d bytes on disk)",
			frame.ByteOffset, frame.ByteSize, frame.Segment, s.size)
	}
	buf := make([]byte, frame.ByteSize)
	if _, err := io.ReadFull(io.NewSectionReader(s.file, frame.ByteOffset, int64(frame.ByteSize)), buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (s *segmentReader) close() {
	if s.file != nil {
		s.file.Close()
	}
	s.name, s.file, s.size = "", nil, 0
}

func (s *segmentReader) Close() { s.close() }

// splitAnnexB returns the NAL unit payloads in an Annex-B byte stream, without
// their start-code prefixes.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				// A start code may be preceded by the fourth zero of a 4-byte
				// prefix, which is not part of the preceding unit.
				if end > start && b[end-1] == 0 {
					end--
				}
				out = append(out, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// spsDimensions reads the coded picture size, in luma samples, out of an H.264
// sequence parameter set. The size has to go into the track header and the
// sample entry, and taking it from the bitstream avoids trusting a configured
// resolution the camera may not have honoured.
func spsDimensions(sps []byte) (int, int, error) {
	r := &bitReader{b: unescapeRBSP(sps)}
	if len(r.b) < 4 {
		return 0, 0, errors.New("SPS too short")
	}
	r.skip(8) // nal_unit_header
	profile := r.bits(8)
	r.skip(8) // constraint flags and reserved bits
	r.skip(8) // level_idc
	r.ue()    // seq_parameter_set_id
	chroma := uint64(1)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma == 3 {
			r.skip(1) // separate_colour_plane_flag
		}
		r.ue()    // bit_depth_luma_minus8
		r.ue()    // bit_depth_chroma_minus8
		r.skip(1) // qpprime_y_zero_transform_bypass_flag
		if r.bits(1) == 1 {
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.bits(1) != 1 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := 8, 8
				for j := 0; j < size && next != 0; j++ {
					next = (last + int(r.se()) + 256) % 256
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.skip(1)
		r.se()
		r.se()
		for n := r.ue(); n > 0; n-- {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.skip(1) // gaps_in_frame_num_value_allowed_flag
	widthMBs := r.ue() + 1
	heightMapUnits := r.ue() + 1
	frameMBsOnly := r.bits(1)
	if frameMBsOnly == 0 {
		r.skip(1) // mb_adaptive_frame_field_flag
	}
	r.skip(1) // direct_8x8_inference_flag
	var cropLeft, cropRight, cropTop, cropBottom uint64
	if r.bits(1) == 1 { // frame_cropping_flag
		cropLeft, cropRight, cropTop, cropBottom = r.ue(), r.ue(), r.ue(), r.ue()
	}
	if r.err != nil {
		return 0, 0, r.err
	}
	// Crop offsets are counted in chroma sampling units: CropUnitX is SubWidthC
	// and CropUnitY is SubHeightC times (2 - frame_mbs_only_flag).
	subWidth, subHeight := uint64(2), uint64(2)
	switch chroma {
	case 0, 3:
		subWidth, subHeight = 1, 1
	case 2:
		subWidth, subHeight = 2, 1
	}
	frameHeightMBs := (2 - frameMBsOnly) * heightMapUnits
	width := int(widthMBs*16 - (cropLeft+cropRight)*subWidth)
	height := int(frameHeightMBs*16 - (cropTop+cropBottom)*subHeight*(2-frameMBsOnly))
	if width <= 0 || height <= 0 || width > 0xFFFF || height > 0xFFFF {
		return 0, 0, fmt.Errorf("SPS yields an impossible picture size %dx%d", width, height)
	}
	return width, height, nil
}

// unescapeRBSP removes H.264 emulation prevention bytes.
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros == 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

type bitReader struct {
	b   []byte
	pos int
	err error
}

func (r *bitReader) bits(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if r.pos >= len(r.b)*8 {
			r.err = io.ErrUnexpectedEOF
			return v
		}
		bit := (r.b[r.pos/8] >> (7 - uint(r.pos%8))) & 1
		v = v<<1 | uint64(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) skip(n int) { r.bits(n) }

func (r *bitReader) ue() uint64 {
	zeros := 0
	for {
		if r.err != nil {
			return 0
		}
		if r.bits(1) == 1 {
			break
		}
		zeros++
		if zeros > 32 {
			r.err = errors.New("malformed exp-Golomb code")
			return 0
		}
	}
	if zeros == 0 {
		return 0
	}
	return (1<<uint(zeros) - 1) + r.bits(zeros)
}

func (r *bitReader) se() int64 {
	v := r.ue()
	if v%2 == 0 {
		return -int64(v / 2)
	}
	return int64((v + 1) / 2)
}

// buildMoov assembles the movie header and returns it with the per-sample
// durations it wrote. All timing lives in stts, which carries an explicit
// duration for every sample, so the variable capture rate survives into the
// container instead of being flattened to an average.
func buildMoov(samples []sample, sps, pps []byte, width, height int) ([]byte, []uint32, error) {
	if len(sps) < 4 {
		return nil, nil, errors.New("SPS too short for a decoder configuration record")
	}
	if len(sps) > 0xFFFF || len(pps) > 0xFFFF {
		return nil, nil, errors.New("parameter set too large for a decoder configuration record")
	}
	durations := make([]uint32, len(samples))
	for i := range samples {
		if i+1 < len(samples) {
			d := samples[i+1].timestamp - samples[i].timestamp
			if d < 0 {
				d = 0
			}
			durations[i] = uint32(d)
			continue
		}
		// The final frame has no successor in the index, so nothing recorded
		// how long it was shown. It is held for the length of the last measured
		// interval: the single value here not read from the index, chosen
		// because a zero-length final sample confuses players, and bounded so
		// the clip's duration still matches the index span to within one frame.
		if i > 0 {
			durations[i] = durations[i-1]
		}
	}
	var total uint64
	for _, d := range durations {
		total += uint64(d)
	}

	var sttsEntries []byte
	runs := 0
	for i := 0; i < len(durations); {
		j := i
		for j < len(durations) && durations[j] == durations[i] {
			j++
		}
		sttsEntries = append(sttsEntries, u32(uint32(j-i))...)
		sttsEntries = append(sttsEntries, u32(durations[i])...)
		runs++
		i = j
	}
	stts := box("stts", concat(u32(0), u32(uint32(runs)), sttsEntries))

	var stssEntries []byte
	syncs := 0
	for i, s := range samples {
		if s.sync {
			stssEntries = append(stssEntries, u32(uint32(i+1))...)
			syncs++
		}
	}
	stss := box("stss", concat(u32(0), u32(uint32(syncs)), stssEntries))

	var stszEntries []byte
	for _, s := range samples {
		if s.size > 0xFFFFFFFF {
			return nil, nil, fmt.Errorf("sample of %d bytes is too large for MP4", s.size)
		}
		stszEntries = append(stszEntries, u32(uint32(s.size))...)
	}
	stsz := box("stsz", concat(u32(0), u32(0), u32(uint32(len(samples))), stszEntries))

	// One sample per chunk keeps the sample-to-chunk map trivial and lets every
	// sample's absolute file offset be stated directly.
	stsc := box("stsc", concat(u32(0), u32(1), u32(1), u32(1), u32(1)))
	var co64Entries []byte
	for _, s := range samples {
		co64Entries = append(co64Entries, u64(uint64(s.offset))...)
	}
	co64 := box("co64", concat(u32(0), u32(uint32(len(samples))), co64Entries))

	avcC := concat(
		[]byte{1, sps[1], sps[2], sps[3], 0xFF, 0xE1},
		u16(uint16(len(sps))), sps,
		[]byte{1},
		u16(uint16(len(pps))), pps,
	)
	compressor := make([]byte, 32)
	compressor[0] = byte(len("WendyOS episode remux"))
	copy(compressor[1:], "WendyOS episode remux")
	avc1 := box("avc1", concat(
		make([]byte, 6), u16(1), // reserved, data_reference_index
		make([]byte, 16), // pre_defined and reserved
		u16(uint16(width)), u16(uint16(height)),
		u32(0x00480000), u32(0x00480000), // 72 dpi horizontal and vertical
		u32(0), u16(1), // reserved, frame_count
		compressor,
		u16(0x18), u16(0xFFFF), // depth, pre_defined
		box("avcC", avcC),
	))
	stsd := box("stsd", concat(u32(0), u32(1), avc1))

	stbl := box("stbl", concat(stsd, stts, stss, stsz, stsc, co64))
	dinf := box("dinf", box("dref", concat(u32(0), u32(1), box("url ", u32(1)))))
	vmhd := box("vmhd", concat(u32(1), u16(0), u16(0), u16(0), u16(0)))
	minf := box("minf", concat(vmhd, dinf, stbl))
	hdlr := box("hdlr", concat(u32(0), u32(0), []byte("vide"), make([]byte, 12), []byte("WendyOS episode camera\x00")))
	mdhd := box("mdhd", concat(
		[]byte{1, 0, 0, 0}, // version 1, so durations are 64-bit
		u64(0), u64(0), u32(playableTimescale), u64(total),
		u16(0x55C4), u16(0), // language "und"
	))
	mdia := box("mdia", concat(mdhd, hdlr, minf))
	tkhd := box("tkhd", concat(
		[]byte{1, 0, 0, 3}, // version 1, track enabled and in movie
		u64(0), u64(0), u32(1), u32(0), u64(total),
		make([]byte, 8), u16(0), u16(0), u16(0), u16(0),
		unityMatrix(),
		u32(uint32(width)<<16), u32(uint32(height)<<16),
	))
	trak := box("trak", concat(tkhd, mdia))
	mvhd := box("mvhd", concat(
		[]byte{1, 0, 0, 0},
		u64(0), u64(0), u32(playableTimescale), u64(total),
		u32(0x00010000), u16(0x0100), u16(0),
		make([]byte, 8),
		unityMatrix(),
		make([]byte, 24),
		u32(2),
	))
	return box("moov", concat(mvhd, trak)), durations, nil
}

func unityMatrix() []byte {
	return concat(u32(0x00010000), u32(0), u32(0), u32(0), u32(0x00010000), u32(0), u32(0), u32(0), u32(0x40000000))
}

func box(kind string, payload ...[]byte) []byte {
	body := concat(payload...)
	out := make([]byte, 0, 8+len(body))
	out = append(out, u32(uint32(8+len(body)))...)
	out = append(out, kind...)
	return append(out, body...)
}

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }
