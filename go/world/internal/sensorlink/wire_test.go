package sensorlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

func manifest(ids ...uint32) *pb.Envelope {
	m := &pb.SensorManifest{}
	for _, id := range ids {
		m.Sensors = append(m.Sensors, &pb.SensorDescriptor{ChannelId: id, InputId: id, Name: "camera", Format: &pb.SensorDescriptor_Video{Video: &pb.VideoFormat{Codec: pb.VideoFormat_MJPEG, Width: 640, Height: 480, Fps: 30}}})
	}
	return &pb.Envelope{Msg: &pb.Envelope_Manifest{Manifest: m}}
}
func datum(channel, seq, chunk uint32, ts uint64, flags uint32, data string) *pb.SensorData {
	return &pb.SensorData{ChannelId: channel, FrameSeq: seq, ChunkSeq: chunk, TsUs: ts, Flags: flags, Payload: []byte(data)}
}
func dataEnvelope(d *pb.SensorData) *pb.Envelope {
	return &pb.Envelope{Msg: &pb.Envelope_Data{Data: d}}
}
func rawEnvelope(raw []byte) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(len(raw)))
	return append(b, raw...)
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(b []byte) (int, error) {
	if len(b) > 2 {
		b = b[:2]
	}
	return w.Buffer.Write(b)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestWireMatchesGeneratedProtobufAndShortIO(t *testing.T) {
	for _, env := range []*pb.Envelope{manifest(1, 2), {Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{1, 2}}}}, dataEnvelope(datum(1, 0, 0, 42, 3, "jpeg")), {Msg: &pb.Envelope_Ping{Ping: &pb.Ping{TsUs: 99}}}} {
		var writer shortWriter
		if err := WriteMessage(&writer, env); err != nil {
			t.Fatal(err)
		}
		got, err := ReadMessage(bytes.NewReader(writer.Bytes()))
		if err != nil || !proto.Equal(got, env) {
			t.Fatal(got, err)
		}
		raw, err := proto.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(writer.Bytes(), rawEnvelope(raw)) {
			t.Fatal("wire differs from generated protobuf")
		}
	}
	if err := WriteMessage(zeroWriter{}, manifest(1)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func TestWireRejectsBoundsTruncationAndAmbiguity(t *testing.T) {
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, MaxFrameBytes+1)
	one, _ := proto.Marshal(manifest(1))
	many := manifest(1).GetManifest()
	for i := uint32(2); i <= MaxManifestChannels+1; i++ {
		many.Sensors = append(many.Sensors, manifest(i).GetManifest().Sensors[0])
	}
	manyRaw, _ := proto.Marshal(&pb.Envelope{Msg: &pb.Envelope_Manifest{Manifest: many}})
	for _, raw := range [][]byte{oversize, {0, 0, 0, 0}, {0, 0, 0, 2, 1}, rawEnvelope([]byte{255}), rawEnvelope(append(append([]byte(nil), one...), one...)), rawEnvelope(manyRaw)} {
		if _, err := ReadMessage(bytes.NewReader(raw)); err == nil {
			t.Fatal("accepted malformed frame")
		}
	}
	if err := WriteMessage(io.Discard, dataEnvelope(datum(1, 1, 0, 1, 3, string(make([]byte, MaxFrameBytes))))); err == nil {
		t.Fatal("oversized write")
	}
}

func TestAssemblerInterleaveMissingWrapAndOwnership(t *testing.T) {
	a, _ := NewAssembler([]uint32{1, 2}, 16)
	feed := func(d *pb.SensorData) *Frame {
		t.Helper()
		f, e := a.Add(d)
		if e != nil {
			t.Fatal(e)
		}
		return f
	}
	if feed(datum(1, math.MaxUint32, 0, 10, FlagKeyframe, "ab")) != nil {
		t.Fatal("partial emitted")
	}
	f := feed(datum(2, 3, 0, 5, 3, "cd"))
	if f == nil || string(f.Payload) != "cd" {
		t.Fatal(f)
	}
	f = feed(datum(1, math.MaxUint32, 1, 10, FlagLastChunk, "ef"))
	if f == nil || string(f.Payload) != "abef" || f.Flags != FlagKeyframe {
		t.Fatal(f)
	}
	d := datum(1, 0, 0, 11, 3, "wrap")
	f = feed(d)
	d.Payload[0] = 'X'
	if f == nil || string(f.Payload) != "wrap" {
		t.Fatal("wrap or owned copy")
	}
	if feed(datum(1, 0, 0, 12, 3, "dup")) != nil {
		t.Fatal("duplicate emitted")
	}
	if feed(datum(1, 1, 0, 12, 1, "a")) != nil {
		t.Fatal("partial emitted")
	}
	if feed(datum(1, 1, 2, 12, 2, "b")) != nil {
		t.Fatal("missing chunk emitted")
	}
	if feed(datum(1, 1, 1, 12, 2, "b")) != nil {
		t.Fatal("dropped frame revived")
	}
	if feed(datum(1, 2, 0, 13, 3, "next")) == nil {
		t.Fatal("did not recover")
	}
	if a.Dropped() < 3 {
		t.Fatal(a.Dropped())
	}
}

func TestAssemblerRejectsUnselectedAndDamagedFrames(t *testing.T) {
	for _, change := range []func(*pb.SensorData){func(d *pb.SensorData) { d.TsUs++ }, func(d *pb.SensorData) { d.FrameSeq++ }, func(d *pb.SensorData) { d.Payload = []byte("toolarge") }} {
		a, _ := NewAssembler([]uint32{1}, 4)
		a.Add(datum(1, 1, 0, 1, 1, "abc"))
		d := datum(1, 1, 1, 1, 2, "d")
		change(d)
		f, _ := a.Add(d)
		if f != nil {
			t.Fatal("damaged frame emitted")
		}
	}
	a, _ := NewAssembler([]uint32{1}, 4)
	for _, d := range []*pb.SensorData{datum(2, 1, 0, 1, 3, "a"), datum(1, 1, 0, 1, 8, "a"), datum(1, 1, 0, 1, 3, "")} {
		if _, err := a.Add(d); err == nil {
			t.Fatal("invalid chunk")
		}
	}
	if _, err := NewAssembler([]uint32{1, 1}, 4); err == nil {
		t.Fatal("duplicate subscription")
	}
}

func TestManifestRejectsAmbiguousUnsupportedSubscriptions(t *testing.T) {
	m := manifest(1, 2).GetManifest()
	if err := ValidateManifest(m, []uint32{1, 2}); err != nil {
		t.Fatal(err)
	}
	m.Sensors[1].InputId = 1
	if err := ValidateManifest(m, []uint32{1, 2}); err == nil {
		t.Fatal("exclusive input accepted")
	}
	if err := ValidateManifest(m, []uint32{3}); err == nil {
		t.Fatal("absent channel accepted")
	}
	m.Sensors[1].ChannelId = 1
	if err := ValidateManifest(m, []uint32{1}); err == nil {
		t.Fatal("duplicate descriptor accepted")
	}
}

func peerHandshake(conn net.Conn, ids ...uint32) error {
	if err := WriteMessage(conn, manifest(ids...)); err != nil {
		return err
	}
	env, err := ReadMessage(conn)
	if err != nil {
		return err
	}
	if env.GetSubscribe() == nil {
		return errors.New("no subscription")
	}
	return nil
}

func TestSessionLatestPerChannelAndEOF(t *testing.T) {
	c, p := net.Pipe()
	defer p.Close()
	peerDone := make(chan error, 1)
	go func() {
		if err := peerHandshake(p, 1, 2); err != nil {
			peerDone <- err
			return
		}
		for seq := uint32(0); seq < 3; seq++ {
			for _, ch := range []uint32{1, 2} {
				if err := WriteMessage(p, dataEnvelope(datum(ch, seq, 0, uint64(seq+1), 3, "x"))); err != nil {
					peerDone <- err
					return
				}
			}
		}
		p.Close()
		peerDone <- nil
	}()
	s, err := Connect(context.Background(), c, []uint32{1, 2}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("read loop did not end")
	}
	for _, ch := range []uint32{1, 2} {
		f, err := s.Next(context.Background())
		if err != nil || f.ChannelID != ch || f.Seq != 2 {
			t.Fatal(f, err)
		}
	}
	if _, err := s.Next(context.Background()); err == nil {
		t.Fatal(err)
	}
	if s.Dropped() != 4 {
		t.Fatal(s.Dropped())
	}
}

func TestSessionHandshakeCancellationAndIdle(t *testing.T) {
	t.Run("cancel handshake", func(t *testing.T) {
		c, p := net.Pipe()
		defer p.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, e := Connect(ctx, c, []uint32{1}, Options{}); done <- e }()
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled handshake succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("cancel blocked")
		}
	})
	t.Run("idle", func(t *testing.T) {
		c, p := net.Pipe()
		defer p.Close()
		go peerHandshake(p, 1)
		s, err := Connect(context.Background(), c, []uint32{1}, Options{IdleTimeout: 20 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err = s.Next(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
	t.Run("unknown channel", func(t *testing.T) {
		c, p := net.Pipe()
		defer p.Close()
		go func() { peerHandshake(p, 1); WriteMessage(p, dataEnvelope(datum(2, 1, 0, 1, 3, "x"))) }()
		s, err := Connect(context.Background(), c, []uint32{1}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err = s.Next(context.Background()); err == nil {
			t.Fatal("unselected channel accepted")
		}
	})
}
