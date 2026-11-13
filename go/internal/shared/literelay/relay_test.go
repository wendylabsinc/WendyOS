package literelay

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type recordedRelay struct {
	messages []*wcomrelaypb.WendyComRelayMessage
}

func (r *recordedRelay) Recv() (*wcomrelaypb.WendyComRelayMessage, error) {
	if len(r.messages) == 0 {
		return nil, io.EOF
	}
	message := r.messages[0]
	r.messages = r.messages[1:]
	return message, nil
}

func TestEstablishedRelaySkipsFutureMessageAndRetainsPayloadBoundary(t *testing.T) {
	// A newer peer adds a length-delimited oneof arm without changing the v1 RPC.
	futureWire := protowire.AppendTag(nil, 15, protowire.BytesType)
	futureWire = protowire.AppendBytes(futureWire, []byte("future control record"))
	var future wcomrelaypb.WendyComRelayMessage
	if err := proto.Unmarshal(futureWire, &future); err != nil {
		t.Fatal(err)
	}
	if future.GetMsg() != nil || len(future.ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("future oneof did not decode as an unknown envelope")
	}
	first := []byte("first inner TLS record")
	second := []byte("second inner TLS record")
	stream := &recordedRelay{messages: []*wcomrelaypb.WendyComRelayMessage{
		&future,
		{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: first}}},
		&future,
		{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: second}}},
	}}
	for _, want := range [][]byte{first, second} {
		got, err := nextRelayPayload(stream)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("payload after future message = %q, %v; want %q", got, err, want)
		}
	}
	if _, err := nextRelayPayload(stream); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v, want EOF", err)
	}
}

func TestEstablishedRelayStillRejectsKnownWrongPhaseAndInvalidPayload(t *testing.T) {
	for name, message := range map[string]*wcomrelaypb.WendyComRelayMessage{
		"open":             {Msg: &wcomrelaypb.WendyComRelayMessage_Open{Open: &wcomrelaypb.WendyComRelayOpen{}}},
		"grant":            {Msg: &wcomrelaypb.WendyComRelayMessage_Grant{Grant: &wcomrelaypb.WendyComRelayGrant{}}},
		"route":            {Msg: &wcomrelaypb.WendyComRelayMessage_Route{Route: &wcomrelaypb.WendyComRelayRoute{}}},
		"established":      {Msg: &wcomrelaypb.WendyComRelayMessage_Established{Established: &wcomrelaypb.WendyComRelayEstablished{}}},
		"nil payload":      {Msg: &wcomrelaypb.WendyComRelayMessage_Payload{}},
		"empty payload":    {Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{}}},
		"oversize payload": {Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: make([]byte, 65537)}}},
	} {
		t.Run(name, func(t *testing.T) {
			if payload, err := nextRelayPayload(&recordedRelay{messages: []*wcomrelaypb.WendyComRelayMessage{message}}); err == nil || payload != nil {
				t.Fatalf("invalid established message returned %d bytes, %v", len(payload), err)
			}
		})
	}
	boundary := bytes.Repeat([]byte{0x16}, 65536)
	stream := &recordedRelay{messages: []*wcomrelaypb.WendyComRelayMessage{
		{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: boundary}}},
	}}
	if got, err := nextRelayPayload(stream); err != nil || !bytes.Equal(got, boundary) {
		t.Fatalf("boundary payload returned %d bytes, %v", len(got), err)
	}
}
