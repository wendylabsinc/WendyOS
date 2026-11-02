package litetopology

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestFutureRowsRemainPartialWithoutCurrentIdentityInterpretation(t *testing.T) {
	rows := []*lite.WendyComNetTopologyRecord{
		{Kind: 99, OwnerRole: 99, Address: []byte{1}, Device: []byte{2}, OwnerPrincipal: "future"},
		{Kind: 1, OwnerRole: 99, Address: []byte{1}, Device: []byte{2}},
		{Kind: 2, OwnerRole: 99, Address: []byte{1}, Device: []byte{2}},
		{Kind: 3, OwnerRole: 99, Address: []byte{1}, Device: []byte{2}},
	}
	known := fixture(t)
	known.Record.LinkId = 77
	calls := 0
	out, err := Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := proto.Clone(known).(*lite.WendyComNetTopologyPage)
		if q.Cursor < uint32(len(rows)) {
			page.Record = rows[q.Cursor]
			page.NextCursor = q.Cursor + 1
		} else {
			page.NextCursor = 0
		}
		return page, nil
	})
	if err != nil || calls != 5 || out == nil || !out.Truncated || len(out.Peers) != 1 || out.Peers[0].LinkId != 77 {
		t.Fatalf("future rows changed known projection: calls=%d out=%v err=%v", calls, out, err)
	}
	if strings.Count(out.GetStatus().GetDetail(), "unsupported")+strings.Count(out.GetStatus().GetDetail(), "Unsupported") > 1 {
		t.Fatalf("per-row detail growth: %q", out.GetStatus().GetDetail())
	}
	empty, err := Read(context.Background(), func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		page := proto.Clone(known).(*lite.WendyComNetTopologyPage)
		page.Record, page.NextCursor = rows[0], 0
		return page, nil
	})
	if err != nil || empty == nil || !empty.Truncated || len(empty.Peers)+len(empty.Routes)+len(empty.Announcements) != 0 {
		t.Fatalf("unknown-only view silently complete: %v %v", empty, err)
	}
}

func TestMissingKindAndMalformedKnownIdentityRemainErrors(t *testing.T) {
	for _, row := range []*lite.WendyComNetTopologyRecord{
		{Kind: 0, OwnerRole: 99},
		{Kind: 1, OwnerRole: 1, Address: []byte{1}, Device: []byte{2}},
		{Kind: 2, OwnerRole: 2, Address: make([]byte, 16), OwnerPrincipal: "future", OwnerInstance: make([]byte, 16)},
		{Kind: 3, OwnerRole: 0, Address: make([]byte, 16), OwnerPrincipal: "not-a-device"},
	} {
		page := fixture(t)
		page.Record, page.NextCursor = row, 0
		if _, err := Read(context.Background(), func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
			return page, nil
		}); err == nil {
			t.Fatalf("known invalid row accepted: %v", row)
		}
	}
	out := new(pb.LocalMeshTopology)
	if err := appendRecord(out, &lite.WendyComNetTopologyRecord{Kind: 99, Address: []byte{1}}); err != nil || !out.Truncated {
		t.Fatalf("future direct projection failed: %v %v", out, err)
	}
}

func compatibilityRawFrame(w io.Writer, category, channel byte, body []byte) error {
	h := []byte{0xa5, 2, category, channel, 0x7f, 0x80, 0, 0}
	binary.BigEndian.PutUint16(h[6:], uint16(len(body)))
	for _, part := range [][]byte{h, body} {
		for len(part) > 0 {
			n, err := w.Write(part)
			if err != nil {
				return err
			}
			if n <= 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}
func compatibilityKnownFrame(w io.Writer, m *lite.WendyComMessage) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	return compatibilityRawFrame(w, 0, 0, b)
}
func compatibilityReadKnownFrame(r io.Reader) (*lite.WendyComMessage, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	b := make([]byte, int(binary.BigEndian.Uint16(h[6:])))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	m := new(lite.WendyComMessage)
	return m, proto.Unmarshal(b, m)
}
func compatibilityUnknownEnvelope() []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, 123, protowire.BytesType), []byte("future subtype"))
}

func TestStreamFutureFramesAndEnvelopeBeforeHandshakeAndPage(t *testing.T) {
	client, device := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	page := fixture(t)
	page.NextCursor = 0
	go func() {
		defer device.Close()
		req, err := compatibilityReadKnownFrame(device)
		if err != nil {
			done <- err
			return
		}
		if err = compatibilityRawFrame(device, 19, 0, nil); err == nil {
			err = compatibilityRawFrame(device, 0, 17, make([]byte, 5000))
		}
		if err == nil {
			err = compatibilityRawFrame(device, 0, 0, compatibilityUnknownEnvelope())
		}
		if err == nil {
			hs := req.GetHandshake()
			hs.Version.Minor = 99
			err = compatibilityKnownFrame(device, &lite.WendyComMessage{Msg: &lite.WendyComMessage_Handshake{Handshake: hs}})
		}
		if err != nil {
			done <- err
			return
		}
		req, err = compatibilityReadKnownFrame(device)
		if err != nil {
			done <- err
			return
		}
		if err = compatibilityRawFrame(device, 3, 0, make([]byte, 65535)); err == nil {
			err = compatibilityRawFrame(device, 0, 0, compatibilityUnknownEnvelope())
		}
		if err == nil {
			err = compatibilityKnownFrame(device, &lite.WendyComMessage{Msg: &lite.WendyComMessage_Response{Response: &lite.WendyComResponse{RequestId: req.GetCommand().RequestId, Data: &lite.WendyComResponse_NetTopology{NetTopology: page}}}})
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := ReadStream(ctx, client)
	if err != nil || out == nil || len(out.Peers) != 1 {
		t.Fatalf("stream did not survive future data: %v %v", out, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnknownRowsRetainPageGenerationCursorAndRawBudgets(t *testing.T) {
	pageFor := func() *lite.WendyComNetTopologyPage {
		page := fixture(t)
		page.Record = &lite.WendyComNetTopologyRecord{Kind: 99, OwnerRole: 99, Address: []byte{1}}
		return page
	}
	calls := 0
	_, err := Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := pageFor()
		if q.Cursor == 0 {
			page.NextCursor = 1
		} else {
			page.Generation++
			page.NextCursor = 0
		}
		return page, nil
	})
	if calls != 4 || status.Code(err) != codes.Unavailable {
		t.Fatalf("generation change bypassed on future row: calls=%d err=%v", calls, err)
	}
	calls = 0
	_, err = Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := pageFor()
		page.NextCursor = 1
		return page, nil
	})
	if calls != 2 || status.Code(err) != codes.DataLoss {
		t.Fatalf("nonadvancing cursor bypassed: calls=%d err=%v", calls, err)
	}
	calls = 0
	_, err = Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := pageFor()
		page.NextCursor = q.Cursor + 1
		return page, nil
	})
	if calls != 1024 || status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("raw future page count unbounded: calls=%d err=%v", calls, err)
	}
	calls = 0
	_, err = Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := pageFor()
		page.NextCursor = q.Cursor + 1
		page.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), make([]byte, 3072)))
		return page, nil
	})
	if calls > 171 || status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("unknown row bytes not counted: calls=%d err=%v", calls, err)
	}
}
