package framesource

// The agent <-> capture-helper wire.
//
// librealsense is a C++ SDK. Linking it into the agent binary would put a C++
// dependency on every WendyOS image whether or not a RealSense is ever
// attached, so the RealSense source runs as a SEPARATE executable the agent
// spawns and owns — the same shape as the gst-launch child the video service
// already supervises, whose raw branch writes frames back down an fd
// (video_raw_tap.go). The agent keeps the hub, the fan-out, the lifecycle and
// the refusals; the helper only captures.
//
// The helper writes length-prefixed protobuf records on stdout and nothing
// else; its logs go to stderr. Two record kinds, in this order:
//
//	RecordSource  exactly one per stream, FIRST — what this capture can promise
//	RecordFrame   zero or more, until the helper exits
//
// The descriptor comes first so the agent can check a subscriber's requirements
// against what the DEVICE actually negotiated rather than what a listing said a
// moment ago. A helper that opened a camera in a degraded mode is refused there
// and then, before a single frame reaches a consumer.

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Record kinds. Explicit values: this is a wire format between two separately
// installed binaries, so an agent and a helper from different releases must
// agree on them.
const (
	RecordSource byte = 1
	RecordFrame  byte = 2
)

// MaxRecordBytes bounds one record. A 1920x1080 BGR colour plane (6.2 MiB)
// beside a 1920x1080 16-bit depth plane (4.0 MiB) is a little over 10 MiB, so
// the cap has room for the largest frame a D4xx can produce while still
// refusing a helper whose framing has gone wrong instead of allocating whatever
// length it asked for.
const MaxRecordBytes = 32 << 20

// WriteRecord writes one length-prefixed record: kind, then a 4-byte
// big-endian length, then the marshalled message.
func WriteRecord(w io.Writer, kind byte, msg proto.Message) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshalling record: %w", err)
	}
	if len(payload) > MaxRecordBytes {
		return fmt.Errorf("record of %d bytes exceeds the %d-byte limit", len(payload), MaxRecordBytes)
	}
	var header [5]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// recordReader reads records from one stream, reusing its payload buffer
// between them. A fresh buffer per record is a frame-sized allocation at
// capture rate -- around 90 MB/s of garbage at 640x480@30 -- competing with
// the H.264 pipeline for the collector on a Pi-class agent. A payload is only
// alive until it is unmarshalled (proto.Unmarshal copies bytes fields, which
// wireReuse_test asserts), so one grown buffer serves every record.
type recordReader struct {
	r   io.Reader
	buf []byte
}

// read returns one record. It returns io.EOF only on a clean boundary — a
// truncated record is io.ErrUnexpectedEOF, because a helper killed mid-frame
// must not look like one that finished. The payload aliases the reader's
// buffer and is valid until the next read.
func (rr *recordReader) read() (kind byte, payload []byte, err error) {
	var header [5]byte
	if _, err := io.ReadFull(rr.r, header[:]); err != nil {
		return 0, nil, err // io.EOF here is a clean end
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > MaxRecordBytes {
		return 0, nil, fmt.Errorf("helper announced a %d-byte record, over the %d-byte limit", n, MaxRecordBytes)
	}
	if uint64(cap(rr.buf)) < uint64(n) {
		rr.buf = make([]byte, n)
	}
	payload = rr.buf[:n]
	if _, err := io.ReadFull(rr.r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return header[0], payload, nil
}

// ReadRecord reads one record from r with a buffer of its own. See
// recordReader.read for the contract.
func ReadRecord(r io.Reader) (kind byte, payload []byte, err error) {
	return (&recordReader{r: r}).read()
}

// ReadSource reads the descriptor record a helper emits first.
func ReadSource(r io.Reader) (*agentpbv2.CalibratedSource, error) {
	return readSource(&recordReader{r: r})
}

func readSource(rr *recordReader) (*agentpbv2.CalibratedSource, error) {
	kind, payload, err := rr.read()
	if err != nil {
		return nil, err
	}
	if kind != RecordSource {
		return nil, fmt.Errorf("helper sent record kind %d before describing its source", kind)
	}
	var src agentpbv2.CalibratedSource
	if err := proto.Unmarshal(payload, &src); err != nil {
		return nil, fmt.Errorf("decoding source descriptor: %w", err)
	}
	return &src, nil
}

// ReadFrame reads one frame record.
func ReadFrame(r io.Reader) (*agentpbv2.CalibratedFrame, error) {
	return readFrame(&recordReader{r: r})
}

func readFrame(rr *recordReader) (*agentpbv2.CalibratedFrame, error) {
	kind, payload, err := rr.read()
	if err != nil {
		return nil, err
	}
	if kind != RecordFrame {
		return nil, fmt.Errorf("helper sent record kind %d where a frame was expected", kind)
	}
	var f agentpbv2.CalibratedFrame
	if err := proto.Unmarshal(payload, &f); err != nil {
		return nil, fmt.Errorf("decoding frame: %w", err)
	}
	return &f, nil
}
