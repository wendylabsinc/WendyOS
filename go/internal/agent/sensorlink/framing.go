package sensorlink

import (
	"encoding/binary"
	"fmt"
	"io"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

// MaxFrameBytes caps a single length-prefixed message (a camera keyframe fits).
const MaxFrameBytes = 8 << 20

// Port is the fixed TCP port a sensorlink-capable device's SensorPairing
// agent listens on. Both the CLI (building the initial SourceAddress on
// `device pair`) and the agent (redialing on boot-resume, where only the
// mDNS-advertised agent port is known) must agree on this value.
const Port = 50060

// WriteMessage writes a 4-byte big-endian length prefix followed by the
// marshaled Envelope.
func WriteMessage(w io.Writer, env *sensorlinkpb.Envelope) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("sensorlink: marshal: %w", err)
	}
	if len(data) > MaxFrameBytes {
		return fmt.Errorf("sensorlink: message %d exceeds cap %d", len(data), MaxFrameBytes)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadMessage reads one length-prefixed Envelope.
func ReadMessage(r io.Reader) (*sensorlinkpb.Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, fmt.Errorf("sensorlink: incoming frame %d exceeds cap %d", n, MaxFrameBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var env sensorlinkpb.Envelope
	if err := proto.Unmarshal(buf, &env); err != nil {
		return nil, fmt.Errorf("sensorlink: unmarshal: %w", err)
	}
	return &env, nil
}
