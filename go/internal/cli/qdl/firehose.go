package qdl

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/archive"
)

// Storage backends a Firehose programmer can be pointed at.
const (
	StorageUFS    = "ufs"
	StorageEMMC   = "emmc"
	StorageNVMe   = "nvme"
	StorageNAND   = "nand"
	StorageSPINOR = "spinor"
)

// Command timeouts. A partition is only acknowledged once every sector of it
// is committed, hence the much longer commit budget.
const (
	firehoseCommandTimeout = 5 * time.Second
	firehoseSetupTimeout   = 10 * time.Second
	firehoseCommitTimeout  = 120 * time.Second
	firehoseDataTimeout    = 60 * time.Second
	// Short, so a programmer that is not listening yet is retried often.
	firehoseWriteTimeout = time.Second
	// How long to keep offering configure after Sahara hands over, and how
	// long each individual attempt waits for an answer.
	firehoseDetectTimeout    = 10 * time.Second
	firehoseConfigureTimeout = 500 * time.Millisecond
	firehosePollTimeout      = 100 * time.Millisecond
)

const (
	// firehoseReadBuffer is far larger than any Firehose message, so a reply
	// cannot be truncated into a fragment that poisons the parse.
	firehoseReadBuffer = 64 * 1024
	// maxPendingBytes bounds the reassembly buffer for a device that streams
	// bytes without ever closing a <data> envelope.
	maxPendingBytes = 1024 * 1024
)

// initialPayloadSize is the payload we propose in the first configure. The
// device answers with the size it actually wants, which we then adopt.
const initialPayloadSize = 1024 * 1024

// maxNegotiablePayload bounds the size a device can talk us into allocating.
const maxNegotiablePayload = 64 * 1024 * 1024

// Session is a Firehose conversation with a programmer running on the device.
type Session struct {
	conn       Conn
	maxPayload int
	log        func(string)
	// pending holds bytes read but not yet consumed: one bulk read can
	// deliver several concatenated messages, or half of one.
	pending []byte
}

// NewSession wraps a transport whose device is already running a Firehose
// programmer. log receives the device's own <log> lines, and may be nil.
func NewSession(conn Conn, log func(string)) *Session {
	if log == nil {
		log = func(string) {}
	}
	return &Session{conn: conn, maxPayload: initialPayloadSize, log: log}
}

// firehoseData is one <data> envelope, which may carry both logs and a response.
type firehoseData struct {
	XMLName xml.Name `xml:"data"`
	Logs    []struct {
		Value string `xml:"value,attr"`
	} `xml:"log"`
	Responses []struct {
		Value               string `xml:"value,attr"`
		RawMode             string `xml:"rawmode,attr"`
		MaxPayload          string `xml:"MaxPayloadSizeToTargetInBytes,attr"`
		MaxPayloadSupported string `xml:"MaxPayloadSizeToTargetInBytesSupported,attr"`
	} `xml:"response"`
}

// response is the outcome of one request.
type response struct {
	ack bool
	// maxPayload is the size the device wants us to use, when it says.
	maxPayload int
	rawMode    bool
}

// Configure negotiates the payload size and points the programmer at a storage
// backend. It must run before any program or patch.
//
// The programmer needs a moment to start after Sahara hands control to it, and
// until it does our writes are cancelled and reads time out. So keep offering
// configure until one is answered, which is also how qdl detects it.
func (s *Session) Configure(storage string) error {
	deadline := time.Now().Add(firehoseDetectTimeout)
	resp, err := s.configureOnce(storage, s.maxPayload)
	for attempts := 1; err != nil; attempts++ {
		if !errors.Is(err, ErrTimeout) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no Firehose programmer answered configure in %s (%d attempts); "+
				"if this board was already used for a flash attempt, power-cycle it back into EDL first: %w",
				firehoseDetectTimeout, attempts, err)
		}
		resp, err = s.configureOnce(storage, s.maxPayload)
	}
	// The device may counter-propose a payload size; adopt it and re-issue, so
	// both ends agree before any bulk data moves.
	if want := resp.maxPayload; want > 0 && want != s.maxPayload {
		if resp, err = s.configureOnce(storage, want); err != nil {
			return fmt.Errorf("re-configuring with the device's payload size: %w", err)
		}
		// Adopt only what the device confirms: writing chunks larger than the
		// size it actually configured truncates a partition mid-transfer.
		if resp.ack && resp.maxPayload > 0 && resp.maxPayload <= want {
			s.maxPayload = resp.maxPayload
		} else if resp.ack {
			s.maxPayload = want
		}
	}
	if !resp.ack {
		s.discard()
		return errors.New("device rejected the Firehose configure request")
	}
	// A retried configure can leave a late duplicate response queued. Firehose
	// has no request ids, so one leftover would make every later await read the
	// previous exchange's answer — and a failed write would report success.
	s.discard()
	return nil
}

// discard consumes whatever the device still has to say and drops it, bounded
// so a programmer that keeps logging cannot wedge the caller.
func (s *Session) discard() {
	deadline := time.Now().Add(firehoseCommandTimeout)
	buf := make([]byte, firehoseReadBuffer)
	for time.Now().Before(deadline) {
		if n, err := s.conn.Read(buf, firehosePollTimeout); n == 0 || err != nil {
			break
		}
	}
	s.pending = nil
}

func (s *Session) configureOnce(storage string, payload int) (*response, error) {
	req := element("configure",
		attr{"MemoryName", storage},
		num("MaxPayloadSizeToTargetInBytes", payload),
		num("Verbose", 0),
		num("ZlpAwareHost", 1),
		num("SkipStorageInit", 0),
	)
	// A write error here is expected while the programmer is still coming
	// up; the read below is what decides whether it is listening.
	if err := s.send(req); err != nil && !errors.Is(err, ErrTimeout) {
		return nil, fmt.Errorf("sending configure: %w", err)
	}
	// A short budget on purpose: Configure retries until firehoseDetectTimeout,
	// and awaiting the full command timeout here would fit only two attempts.
	return s.await(firehoseConfigureTimeout)
}

// SectorsFor reports how many sectors of size bytes an entry writes: what
// remains of the payload after its offset, rounded up. Counting from the file
// size instead would stream file_sector_offset sectors of padding past the end
// of the region.
func SectorsFor(e ProgramEntry, size int64) (uint32, error) {
	if e.SectorSize == 0 {
		return 0, fmt.Errorf("entry %q has no sector size", e.Label)
	}
	if size < 0 {
		return 0, fmt.Errorf("payload %s has a negative size", e.Filename)
	}
	// uint64 throughout: the descriptor's uint32 offset times its uint32 sector
	// size overflows int64, which used to wrap positive and pass every check.
	offset := uint64(e.FileOffset) * uint64(e.SectorSize)
	if offset >= uint64(size) {
		return 0, fmt.Errorf("file_sector_offset %d is past the end of %s", e.FileOffset, e.Filename)
	}
	avail := uint64(size) - offset
	total := (avail + uint64(e.SectorSize) - 1) / uint64(e.SectorSize)
	if total > uint64(^uint32(0)) {
		return 0, fmt.Errorf("payload %s needs %d sectors, more than the protocol can address", e.Filename, total)
	}
	if e.NumSectors != 0 && uint32(total) > e.NumSectors {
		return 0, fmt.Errorf("payload %s needs %d sectors but partition %q holds only %d",
			e.Filename, total, e.Label, e.NumSectors)
	}
	return uint32(total), nil
}

// Program writes one partition, streaming the payload named by the entry from
// dir. Progress is reported in bytes written of bytes to write.
func (s *Session) Program(ctx context.Context, dir string, e ProgramEntry, progress func(done, total int64)) error {
	path, err := archive.SafeJoin(dir, e.Filename)
	if err != nil || path == "" {
		return fmt.Errorf("flash descriptor names an unsafe payload path %q", e.Filename)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening payload: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sizing payload: %w", err)
	}

	sectorSize := e.SectorSize
	sectors, err := SectorsFor(e, info.Size())
	if err != nil {
		return err
	}

	req := element("program",
		num("SECTOR_SIZE_IN_BYTES", sectorSize),
		num("num_partition_sectors", sectors),
		num("physical_partition_number", e.Partition),
		attr{"start_sector", e.StartSector},
		attr{"filename", e.Filename},
	)
	if err := s.send(req); err != nil {
		return fmt.Errorf("sending program request for %q: %w", e.Label, err)
	}
	resp, err := s.await(firehoseSetupTimeout)
	if err != nil {
		return fmt.Errorf("setting up %q: %w", e.Label, err)
	}
	if !resp.ack {
		return fmt.Errorf("device refused to program %q", e.Label)
	}

	if _, err := f.Seek(int64(e.FileOffset)*int64(sectorSize), io.SeekStart); err != nil {
		return fmt.Errorf("seeking payload for %q: %w", e.Label, err)
	}
	if err := s.stream(ctx, f, sectors, sectorSize, e.Label, progress); err != nil {
		return err
	}

	// The device acknowledges the partition only once every sector is
	// committed, so this is where a genuine write failure surfaces.
	resp, err = s.await(firehoseCommitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for %q to commit: %w", e.Label, err)
	}
	if !resp.ack {
		return fmt.Errorf("device reported that programming %q failed", e.Label)
	}
	return nil
}

// stream sends sectors worth of r in payload-sized chunks. Every write is a
// whole number of sectors, so a short final read is zero-padded: the programmer
// expects exactly the byte count implied by the sector maths.
func (s *Session) stream(ctx context.Context, r io.Reader, sectors, sectorSize uint32, label string, progress func(done, total int64)) error {
	perChunk := uint32(s.maxPayload) / sectorSize
	if perChunk == 0 {
		return fmt.Errorf("negotiated payload of %d bytes cannot hold one %d-byte sector",
			s.maxPayload, sectorSize)
	}
	buf := make([]byte, perChunk*sectorSize)

	for left := sectors; left > 0; {
		// A single partition is minutes of transfer, so an abort has to be
		// honoured between chunks rather than only between partitions.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing %q was cancelled: %w", label, err)
		}
		chunk := min(perChunk, left)
		want := chunk * sectorSize
		n, err := io.ReadFull(r, buf[:want])
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading payload for %q: %w", label, err)
		}
		for i := n; i < int(want); i++ {
			buf[i] = 0
		}
		if _, err := s.conn.Write(buf[:want], firehoseDataTimeout); err != nil {
			// The device usually explains itself; surface that rather
			// than only the USB-level failure.
			if resp, rerr := s.await(30 * time.Second); rerr == nil && !resp.ack {
				return fmt.Errorf("writing %q failed and the device NAKed the transfer: %w", label, err)
			}
			return fmt.Errorf("writing payload for %q: %w", label, err)
		}
		left -= chunk
		if progress != nil {
			progress(int64(sectors-left)*int64(sectorSize), int64(sectors)*int64(sectorSize))
		}
	}
	return nil
}

// Patch applies one in-place edit to the device's storage. The value and
// start_sector are passed through verbatim because they may be expressions the
// programmer resolves against the real disk geometry.
func (s *Session) Patch(p PatchEntry) error {
	req := element("patch",
		num("SECTOR_SIZE_IN_BYTES", p.SectorSize),
		num("byte_offset", p.ByteOffset),
		attr{"filename", p.Filename},
		num("physical_partition_number", p.Partition),
		num("size_in_bytes", p.SizeInBytes),
		attr{"start_sector", p.StartSector},
		attr{"value", p.Value},
	)
	if err := s.send(req); err != nil {
		return fmt.Errorf("sending patch %q: %w", p.What, err)
	}
	resp, err := s.await(firehoseCommandTimeout)
	if err != nil {
		return fmt.Errorf("applying patch %q: %w", p.What, err)
	}
	if !resp.ack {
		return fmt.Errorf("device refused patch %q", p.What)
	}
	return nil
}

// Reset asks the device to reboot. The delay mirrors qdl: resetting the instant
// the command lands has been seen to leave the device failing to come back up.
func (s *Session) Reset() error {
	req := element("power", attr{"value", "reset"}, num("DelayInSeconds", 10))
	if err := s.send(req); err != nil {
		return fmt.Errorf("sending reset: %w", err)
	}
	resp, err := s.await(firehoseCommandTimeout)
	if err != nil {
		return fmt.Errorf("requesting device reset: %w", err)
	}
	if !resp.ack {
		return errors.New("device refused the reset request")
	}
	// Drain whatever the device says on its way down so a later session does
	// not read this one's trailing logs.
	_, _ = s.await(time.Second)
	return nil
}

// attr is one attribute of a request element; order is preserved.
type attr struct {
	name  string
	value string
}

func num[T int | uint32](name string, v T) attr {
	return attr{name, strconv.FormatInt(int64(v), 10)}
}

// element renders one self-closing request element. Self-closing is required:
// the programmer's XML parser rejects the expanded <tag></tag> form that Go's
// encoding/xml always emits. Values are escaped as they come from a download.
func element(name string, attrs ...attr) string {
	var b bytes.Buffer
	b.WriteString("<" + name)
	for _, a := range attrs {
		b.WriteString(" " + a.name + `="`)
		_ = xml.EscapeText(&b, []byte(a.value))
		b.WriteString(`"`)
	}
	b.WriteString("/>")
	return b.String()
}

// send wraps one request element in a <data> envelope and writes it.
func (s *Session) send(cmd string) error {
	var buf bytes.Buffer
	buf.WriteString("<?xml version=\"1.0\" ?>\n<data>\n  ")
	buf.WriteString(cmd)
	buf.WriteString("\n</data>\n")

	// A device that still owes us log messages will not accept a write until
	// they are drained, so clear the backlog and retry once.
	_, err := s.conn.Write(buf.Bytes(), firehoseWriteTimeout)
	if errors.Is(err, ErrTimeout) {
		_, _ = s.await(firehosePollTimeout)
		_, err = s.conn.Write(buf.Bytes(), firehoseWriteTimeout)
	}
	return err
}

// await consumes device messages until it has a response and the device has
// gone quiet, or the budget runs out. Draining fully matters: a leftover <log>
// would block the next write and surface as an unrelated timeout.
func (s *Session) await(budget time.Duration) (*response, error) {
	deadline := time.Now().Add(budget)
	buf := make([]byte, firehoseReadBuffer)
	var resp *response

	for {
		// Checked every pass, not only when the device falls quiet: one that
		// keeps sending logs and never answers would otherwise spin forever.
		if time.Now().After(deadline) {
			if resp != nil {
				return resp, nil
			}
			return nil, fmt.Errorf("no Firehose response within %s: %w", budget, ErrTimeout)
		}
		n, err := s.conn.Read(buf, firehosePollTimeout)
		switch {
		case err != nil && !errors.Is(err, ErrTimeout):
			// Return what we have: on a reset the link drops right
			// after the ACK, and losing it would look like a failure.
			if resp != nil {
				return resp, nil
			}
			return nil, err
		case n == 0 || errors.Is(err, ErrTimeout):
			if resp != nil {
				return resp, nil
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("no Firehose response within %s: %w", budget, ErrTimeout)
			}
			continue
		}

		s.pending = append(s.pending, buf[:n]...)
		if len(s.pending) > maxPendingBytes {
			s.pending = nil
			return nil, fmt.Errorf("device sent %d bytes with no complete message", maxPendingBytes)
		}
		for _, msg := range s.takeEnvelopes() {
			r, err := s.consume(msg)
			if err != nil {
				return nil, err
			}
			if r == nil {
				continue
			}
			resp = r
			// In raw mode the device stops speaking XML, so there is
			// nothing further to drain.
			if r.rawMode {
				return resp, nil
			}
		}
	}
}

// sanitiseDeviceText strips control bytes from text the device sent, which is
// printed to a terminal and must not be able to carry escape sequences.
func sanitiseDeviceText(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return r
		// C0, DEL and C1: 0x9b and 0x9d are the 8-bit CSI and OSC
		// introducers, so stripping only the 7-bit ESC leaves the door open.
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return -1
		// Bidi overrides can disguise what a line says.
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, v)
}

// takeEnvelopes splits the buffered bytes into complete <data> envelopes,
// leaving any partial trailing message pending.
func (s *Session) takeEnvelopes() [][]byte {
	const closing = "</data>"
	var out [][]byte
	start := 0
	for {
		i := bytes.Index(s.pending[start:], []byte(closing))
		if i < 0 {
			break
		}
		end := start + i + len(closing)
		out = append(out, bytes.Clone(s.pending[start:end]))
		start = end
	}
	if start > 0 {
		s.pending = bytes.Clone(s.pending[start:])
	}
	return out
}

// consume parses one envelope, logging its <log> lines. It returns nil when the
// envelope held no response.
func (s *Session) consume(msg []byte) (*response, error) {
	var doc firehoseData
	if err := xml.Unmarshal(msg, &doc); err != nil {
		// Firehose programmers emit stray non-XML noise on some targets;
		// dropping it is safer than aborting a flash mid-partition.
		s.log(fmt.Sprintf("unparseable device message: %q", bytes.TrimSpace(msg)))
		return nil, nil
	}
	for _, l := range doc.Logs {
		s.log(sanitiseDeviceText(l.Value))
	}
	if len(doc.Responses) == 0 {
		return nil, nil
	}
	// If several arrive at once the last one is the outcome of our request.
	last := doc.Responses[len(doc.Responses)-1]
	r := &response{ack: last.Value == "ACK", rawMode: last.RawMode == "true"}
	// On an ACK the device reports the size it can actually take, which may
	// be larger than the one it echoes back.
	for _, field := range []string{last.MaxPayloadSupported, last.MaxPayload} {
		if field == "" {
			continue
		}
		v, err := strconv.Atoi(field)
		if err != nil || v <= 0 || v > maxNegotiablePayload {
			continue
		}
		r.maxPayload = v
		break
	}
	return r, nil
}
