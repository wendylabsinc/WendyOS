// Package sensorlink implements the framed protobuf transport used by the
// Wendy SensorLink TCP simulator. It does not authenticate a remote device.
package sensorlink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const MaxFrameBytes = 8 << 20
const MaxChannels = 8
const MaxManifestChannels = 64
const Port = 50060

// ReadMessage uses the upstream four-byte big-endian length and Envelope type.
// Length is checked before allocation; empty or unknown-only envelopes fail.
func ReadMessage(r io.Reader) (*pb.Envelope, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxFrameBytes {
		return nil, fmt.Errorf("sensorlink message length %d outside bounds", n)
	}
	raw := make([]byte, int(n))
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, err
	}
	if err := preflight(raw); err != nil {
		return nil, err
	}
	var envelope pb.Envelope
	if err := (proto.UnmarshalOptions{RecursionLimit: 16}).Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Msg == nil {
		return nil, errors.New("sensorlink envelope has no supported message")
	}
	return &envelope, nil
}

func WriteMessage(w io.Writer, envelope *pb.Envelope) error {
	if envelope == nil || envelope.Msg == nil {
		return errors.New("sensorlink envelope requires a message")
	}
	if proto.Size(envelope) > MaxFrameBytes {
		return errors.New("sensorlink message exceeds bounds")
	}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > MaxFrameBytes {
		return errors.New("sensorlink message exceeds bounds")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if err = writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, raw)
}

// Check repetition and small control-message limits before protobuf decoding
// can allocate descriptor objects for an adversarial manifest.
func preflight(raw []byte) error {
	count := 0
	for len(raw) > 0 {
		number, kind, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return errors.New("invalid sensorlink protobuf tag")
		}
		raw = raw[n:]
		if number >= 1 && number <= 4 {
			count++
			if count > 1 || kind != protowire.BytesType {
				return errors.New("ambiguous sensorlink envelope")
			}
			body, n := protowire.ConsumeBytes(raw)
			if n < 0 {
				return errors.New("invalid sensorlink protobuf body")
			}
			if number != 3 && len(body) > 64<<10 {
				return errors.New("sensorlink control message exceeds bounds")
			}
			if number == 1 {
				descriptors := 0
				for len(body) > 0 {
					tag, typ, used := protowire.ConsumeTag(body)
					if used < 0 {
						return errors.New("invalid manifest tag")
					}
					body = body[used:]
					if tag == 1 {
						if typ != protowire.BytesType {
							return errors.New("invalid descriptor encoding")
						}
						b, size := protowire.ConsumeBytes(body)
						if size < 0 || len(b) > 4096 {
							return errors.New("descriptor exceeds bounds")
						}
						descriptors++
						if descriptors > MaxManifestChannels {
							return errors.New("manifest channel count exceeds bounds")
						}
						body = body[size:]
					} else {
						size := protowire.ConsumeFieldValue(tag, typ, body)
						if size < 0 {
							return errors.New("invalid manifest field")
						}
						body = body[size:]
					}
				}
			}
			raw = raw[n:]
		} else {
			size := protowire.ConsumeFieldValue(number, kind, raw)
			if size < 0 {
				return errors.New("invalid envelope field")
			}
			raw = raw[size:]
		}
	}
	if count != 1 {
		return errors.New("sensorlink envelope has no supported message")
	}
	return nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
