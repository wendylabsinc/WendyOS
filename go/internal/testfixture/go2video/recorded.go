package go2video

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Recording contains exact CDR samples admitted by the archive verifier. It is
// evidence of replay mechanics, not authentication of an unauthenticated robot.
type Recording struct {
	Payloads     [][]byte
	Intervals    []time.Duration
	Reference    [][32]byte
	ManifestHash string
}

type recordedManifest struct {
	SchemaVersion int `json:"schema_version"`
	Samples       []struct {
		File    string `json:"file"`
		SHA256  string `json:"sha256"`
		Bytes   int    `json:"bytes"`
		Receipt int64  `json:"received_monotonic_ns"`
	} `json:"samples"`
}

func LoadRecording(ctx context.Context, directory string) (*Recording, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(directory), "replay.json"))
	if err != nil {
		return nil, err
	}
	if len(raw) > 2<<20 {
		return nil, fmt.Errorf("replay manifest exceeds bound")
	}
	var manifest recordedManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 || len(manifest.Samples) < 30 || len(manifest.Samples) > 1800 {
		return nil, fmt.Errorf("invalid admitted replay manifest")
	}
	sum := sha256.Sum256(raw)
	recording := &Recording{ManifestHash: hex.EncodeToString(sum[:])}
	var source bytes.Buffer
	total := 0
	for index, sample := range manifest.Samples {
		if sample.File != fmt.Sprintf("%04d.cdr", index) || sample.Bytes < 24 || sample.Bytes > 1<<20 {
			return nil, fmt.Errorf("invalid replay member")
		}
		path := filepath.Join(directory, sample.File)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(sample.Bytes) {
			return nil, fmt.Errorf("replay member is not the admitted regular file")
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		total += len(payload)
		digest := sha256.Sum256(payload)
		if total > 64<<20 || hex.EncodeToString(digest[:]) != sample.SHA256 {
			return nil, fmt.Errorf("replay member checksum/budget mismatch")
		}
		// Independent extraction uses the recorder's fixed observed CDR contract.
		// The production parser is exercised separately on these unchanged bytes.
		n := int(uint32(payload[16]) | uint32(payload[17])<<8 | uint32(payload[18])<<16 | uint32(payload[19])<<24)
		if n < 5 || n > len(payload)-20 {
			return nil, fmt.Errorf("invalid recorded H264 length")
		}
		source.Write(payload[20 : 20+n])
		recording.Payloads = append(recording.Payloads, payload)
		interval := time.Second / 30
		if index > 0 {
			interval = time.Duration(sample.Receipt - manifest.Samples[index-1].Receipt)
			if interval <= 0 || interval > time.Second {
				return nil, fmt.Errorf("recording gap outside replay pacing bound")
			}
		}
		recording.Intervals = append(recording.Intervals, interval)
	}
	recording.Reference, err = DecodeFrameHashes(ctx, source.Bytes(), 1800)
	if err != nil {
		return nil, fmt.Errorf("independent source decode: %w", err)
	}
	distinct := map[[32]byte]bool{}
	for _, hash := range recording.Reference {
		distinct[hash] = true
	}
	if len(recording.Reference) < 30 || len(distinct) < 20 {
		return nil, fmt.Errorf("recording lacks 30 decoded frames/20 distinct source frames")
	}
	return recording, nil
}

// DecodeFrameHashes streams independently decoded RGB frames without retaining
// a full uncompressed recording. The fixed caps reject unsupported geometry.
func DecodeFrameHashes(ctx context.Context, stream []byte, limit int) ([][32]byte, error) {
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", "-q", "fdsrc", "fd=0", "!", "h264parse", "!", "avdec_h264", "!", "videoconvert", "!", "video/x-raw,format=RGB,width=640,height=360", "!", "fdsink", "fd=1")
	cmd.Stdin = bytes.NewReader(stream)
	cmd.WaitDelay = time.Second
	var diagnostic boundedDiagnostic
	cmd.Stderr = &diagnostic
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	joined := false
	defer func() {
		if !joined {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	frame := make([]byte, Width*Height*3)
	var hashes [][32]byte
	for {
		_, err = io.ReadFull(out, frame)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("partial decoded RGB frame: %w", err)
		}
		if len(hashes) >= limit {
			return nil, fmt.Errorf("decoded frame count exceeds bound")
		}
		hashes = append(hashes, sha256.Sum256(frame))
	}
	err = cmd.Wait()
	joined = true
	if err != nil {
		return nil, fmt.Errorf("H264 decoder failed: %w: %.1024s", err, diagnostic.String())
	}
	return hashes, nil
}

// MatchRecording requires fresh decoded content in original recording order.
// Capture may drop producer frames or repeat the last kernel buffer. A capture
// cannot move backwards except at the end of a complete replay cycle.
func MatchRecording(reference, captured [][32]byte) error {
	if len(captured) < 6 || len(reference) < 30 {
		return fmt.Errorf("recorded replay decoded too few frames")
	}
	distinct := map[[32]byte]bool{}
	for _, hash := range captured {
		distinct[hash] = true
	}
	if len(distinct) < 3 {
		return fmt.Errorf("recorded replay did not advance through distinct frames")
	}
	var candidates []int
	for index, hash := range reference {
		if hash == captured[0] {
			candidates = append(candidates, index)
		}
	}
	for _, hash := range captured[1:] {
		next := map[int]bool{}
		for _, last := range candidates {
			// Bound forward loss to a quarter of the corpus and at most60 frames.
			// A short corpus must not make every backwards step look like a wrap.
			for delta := 0; delta <= min(60, len(reference)/4); delta++ {
				index := (last + delta) % len(reference)
				if reference[index] == hash {
					next[index] = true
				}
			}
		}
		candidates = candidates[:0]
		for index := range next {
			candidates = append(candidates, index)
		}
		if len(candidates) == 0 {
			return fmt.Errorf("decoded capture is not ordered source content")
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("decoded capture does not match recording")
	}
	return nil
}

// Decoder diagnostics are bounded even for a damaged archived bitstream.
type boundedDiagnostic struct{ buffer bytes.Buffer }

func (b *boundedDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (64 << 10) - b.buffer.Len(); remaining > 0 {
		_, _ = b.buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

func (b *boundedDiagnostic) String() string { return b.buffer.String() }
