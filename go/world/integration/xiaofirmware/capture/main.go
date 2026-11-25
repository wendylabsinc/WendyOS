// Observe genuine SensorLink metadata through the normal Wendy Lite client.
// This tool is private, PTY-only and cannot flash firmware or connect over TLS.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/jpeg"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/encoding/protojson"
)

const maxFrameBytes = 8 << 20
const maxCaptureBytes = 512 << 20
const maxRawBytes = 16 << 20

type chunk struct {
	Channel   uint32 `json:"channel_id"`
	Sequence  uint32 `json:"frame_seq"`
	Index     uint32 `json:"chunk_seq"`
	Timestamp uint64 `json:"timestamp_us"`
	Flags     uint32 `json:"flags"`
	Payload   []byte `json:"payload"`
}

type assembled struct {
	Sequence  uint32
	Timestamp uint64
	Payload   []byte
}

type assembler struct {
	partial    *assembled
	next       uint32
	seen       bool
	sequence   uint32
	timestamp  uint64
	Incomplete uint64
	Rejected   uint64
}

func (a *assembler) add(c chunk) *assembled {
	if len(c.Payload) == 0 || len(c.Payload) > maxFrameBytes || c.Flags & ^uint32(3) != 0 {
		a.partial = nil
		a.Rejected++
		return nil
	}
	if c.Index == 0 {
		if a.partial != nil {
			a.Incomplete++
			a.partial = nil
		}
		if a.seen {
			delta := c.Sequence - a.sequence
			if delta == 0 || delta >= 1<<31 || c.Timestamp <= a.timestamp {
				a.Rejected++
				return nil
			}
		}
		a.seen, a.sequence, a.timestamp = true, c.Sequence, c.Timestamp
		a.partial = &assembled{Sequence: c.Sequence, Timestamp: c.Timestamp, Payload: append([]byte(nil), c.Payload...)}
		a.next = 1
	} else {
		if a.partial == nil || c.Sequence != a.partial.Sequence || c.Timestamp != a.partial.Timestamp || c.Index != a.next || len(c.Payload) > maxFrameBytes-len(a.partial.Payload) {
			a.partial = nil
			a.Rejected++
			return nil
		}
		a.partial.Payload = append(a.partial.Payload, c.Payload...)
		a.next++
	}
	if c.Flags&2 != 0 {
		result := a.partial
		a.partial = nil
		return result
	}
	return nil
}

func verifyPTY(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^/dev/(ttys[0-9]+|pts/[0-9]+)$`).MatchString(resolved) {
		return errors.New("only an emulator's local PTY is accepted")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("PTY must be a character device")
	}
	return nil
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

func run(path, out string, channel uint32, duration time.Duration, stdoutJPEG bool) (resultErr error) {
	if duration <= 0 || duration > 2*time.Minute {
		return errors.New("duration must be between zero and two minutes")
	}
	if err := verifyPTY(path); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(out, "frames"), 0700); err != nil {
		return err
	}
	raw, err := os.OpenFile(filepath.Join(out, "chunks.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer raw.Close()
	frames, err := os.OpenFile(filepath.Join(out, "frames.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer frames.Close()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	session := hex.EncodeToString(nonce[:])
	summary := map[string]any{"origin": "simulated", "session_id": session, "transport": "WendyCom over emulator PTY", "firmware_execution_verified": false, "capture_complete": false}
	defer func() {
		if resultErr != nil {
			summary["error"] = resultErr.Error()
		}
		_ = writeJSON(filepath.Join(out, "capture-summary.json"), summary)
	}()
	client := liteclient.NewWendyLiteClient()
	if err = client.ConnectToSerial(path); err != nil {
		return fmt.Errorf("serial handshake: %w", err)
	}
	defer client.Close()
	info, err := client.GetDeviceInfo(5 * time.Second)
	if err != nil {
		return fmt.Errorf("get device info: %w", err)
	}
	if err = writeJSON(filepath.Join(out, "device-info.json"), info); err != nil {
		return err
	}
	manifest, err := client.GetSensorManifest(5 * time.Second)
	if err != nil {
		return err
	}
	mb, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(manifest)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(out, "sensor-manifest.json"), mb, 0600); err != nil {
		return err
	}
	var width, height uint32
	for _, s := range manifest.GetSensors() {
		if s.GetChannelId() == channel && s.GetVideo().GetCodec() == pb.VideoFormat_MJPEG {
			width, height = s.GetVideo().GetWidth(), s.GetVideo().GetHeight()
		}
	}
	if width == 0 || height == 0 || uint64(width)*uint64(height) > 16<<20 {
		return errors.New("selected MJPEG channel missing or dimensions invalid")
	}
	summary["channels"] = []map[string]uint32{{"channel_id": channel, "width": width, "height": height}}
	queue := make(chan chunk, 128)
	var overflow, received atomic.Uint64
	remove := client.AddSensorDataListener(func(d *pb.SensorData) {
		if d.GetChannelId() != channel {
			return
		}
		received.Add(1)
		if len(d.GetPayload()) > maxFrameBytes {
			overflow.Add(1)
			return
		}
		c := chunk{Channel: d.GetChannelId(), Sequence: d.GetFrameSeq(), Index: d.GetChunkSeq(), Timestamp: d.GetTsUs(), Flags: d.GetFlags(), Payload: append([]byte(nil), d.GetPayload()...)}
		select {
		case queue <- c:
		default:
			overflow.Add(1)
		}
	})
	defer remove()
	if err = client.SensorLinkSubscribe([]uint32{channel}, 5*time.Second); err != nil {
		return err
	}
	defer client.SensorLinkUnsubscribe([]uint32{channel}, time.Second)
	frameWriter := json.NewEncoder(frames)
	var a assembler
	var count, invalidJPEG, totalBytes, rawBytes, rawOmitted uint64
	timer := time.NewTimer(duration)
	defer timer.Stop()
	defer func() {
		if a.partial != nil {
			a.Incomplete++
		}
		summary["received_chunks"] = received.Load()
		summary["observer_queue_drops"] = overflow.Load()
		summary["incomplete_frames"] = a.Incomplete
		summary["rejected_chunks"] = a.Rejected
		summary["rejected_jpegs"] = invalidJPEG
		summary["frames"] = count
		summary["payload_bytes"] = totalBytes
		summary["raw_chunk_bytes"] = rawBytes
		summary["raw_chunks_omitted"] = rawOmitted
		_ = raw.Sync()
		_ = frames.Sync()
	}()
	for {
		select {
		case <-client.Done():
			return errors.New("firmware connection closed")
		case <-timer.C:
			summary["capture_complete"] = true
			if count == 0 {
				return errors.New("no complete JPEG frames received")
			}
			if overflow.Load() != 0 {
				return errors.New("observer could not retain every received chunk")
			}
			return nil
		case c := <-queue:
			totalBytes += uint64(len(c.Payload))
			if totalBytes > maxCaptureBytes {
				return errors.New("capture byte budget exceeded")
			}
			encoded, marshalErr := json.Marshal(c)
			if marshalErr != nil {
				return marshalErr
			}
			if rawBytes+uint64(len(encoded))+1 <= maxRawBytes && rawOmitted == 0 {
				_, err = raw.Write(append(encoded, '\n'))
				rawBytes += uint64(len(encoded)) + 1
			} else {
				rawOmitted++
			}
			if err != nil {
				return err
			}
			f := a.add(c)
			if f == nil {
				continue
			}
			if !bytes.HasPrefix(f.Payload, []byte{255, 216}) || !bytes.HasSuffix(f.Payload, []byte{255, 217}) {
				invalidJPEG++
				continue
			}
			config, decodeErr := jpeg.DecodeConfig(bytes.NewReader(f.Payload))
			if decodeErr != nil || config.Width != int(width) || config.Height != int(height) {
				invalidJPEG++
				continue
			}
			if _, decodeErr = jpeg.Decode(bytes.NewReader(f.Payload)); decodeErr != nil {
				invalidJPEG++
				continue
			}
			digest := sha256.Sum256(f.Payload)
			name := fmt.Sprintf("frames/frame-%08d.jpg", count)
			if err = os.WriteFile(filepath.Join(out, name), f.Payload, 0600); err != nil {
				return err
			}
			row := map[string]any{"session_id": session, "channel_id": channel, "frame_seq": f.Sequence, "timestamp_us": f.Timestamp, "received_unix_ns": time.Now().UnixNano(), "jpeg_path": name, "sha256": hex.EncodeToString(digest[:]), "width": width, "height": height}
			if err = frameWriter.Encode(row); err != nil {
				return err
			}
			count++
			if stdoutJPEG {
				if _, err = os.Stdout.Write(f.Payload); err != nil {
					return fmt.Errorf("write JPEG stdout: %w", err)
				}
			}
		}
	}
}

func main() {
	// A stream consumer may close early. Return EPIPE so deferred capture
	// receipts record that interruption instead of terminating on SIGPIPE.
	signal.Ignore(syscall.SIGPIPE)
	path := flag.String("serial-pty", "", "emulator PTY, never a physical serial device")
	out := flag.String("out", "", "fresh private capture directory")
	channel := flag.Uint("channel", 0, "MJPEG SensorLink channel")
	duration := flag.Duration("duration", 15*time.Second, "bounded capture duration, maximum 2m")
	stdoutJPEG := flag.Bool("stdout-jpeg", false, "write original complete JPEG bytes to stdout; retain metadata in --out")
	flag.Parse()
	if *path == "" || *out == "" || uint64(*channel) > 1<<32-1 {
		fmt.Fprintln(os.Stderr, "require --serial-pty, --out and valid --channel")
		os.Exit(2)
	}
	if err := run(*path, *out, uint32(*channel), *duration, *stdoutJPEG); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
