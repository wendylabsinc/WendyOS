package liteclient

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func compatibilityFrame(category, channel byte, body []byte) []byte {
	frame := make([]byte, headerSize+len(body))
	frame[0], frame[1], frame[2], frame[3] = headerMagic, headerVersion, category, channel
	binary.BigEndian.PutUint16(frame[6:8], uint16(len(body)))
	copy(frame[headerSize:], body)
	return frame
}

func futureMessageBody() []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), []byte("future"))
}

func TestDirectLinkHandshakeSkipsFutureMessages(t *testing.T) {
	client, device := net.Pipe()
	t.Cleanup(func() { client.Close(); device.Close() })
	serverDone := make(chan error, 1)
	go func() {
		link := newDirectLink(device)
		request, err := link.recv(time.Second)
		if err != nil {
			serverDone <- err
			return
		}
		reply := &wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Handshake{
			Handshake: &wendypb.WendyComHandshake{
				HandshakeId: request.GetHandshake().GetHandshakeId(),
				Version:     &wendypb.WendyComProtocolVersion{Major: versionMajor, Minor: versionMinor + 1},
			},
		}}
		reply.ProtoReflect().SetUnknown(futureMessageBody())
		reply.GetHandshake().ProtoReflect().SetUnknown(futureMessageBody())
		reply.GetHandshake().GetVersion().ProtoReflect().SetUnknown(futureMessageBody())
		body, err := proto.Marshal(reply)
		if err != nil {
			serverDone <- err
			return
		}
		// Future frame categories/channels may legitimately have no body;
		// a future top-level protobuf variant has no known Msg discriminator.
		frames := append(compatibilityFrame(1, 0, nil), compatibilityFrame(0, 1, nil)...)
		frames = append(frames, compatibilityFrame(1, 0, []byte("opaque future body"))...)
		frames = append(frames, compatibilityFrame(0, 0, futureMessageBody())...)
		frames = append(frames, compatibilityFrame(0, 0, body)...)
		_, err = device.Write(frames)
		serverDone <- err
	}()
	c := &WendyLiteClient{link: newDirectLink(client)}
	if err := c.handshake(); err != nil {
		t.Fatalf("handshake after extension messages: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if c.peerProtocolVersion.Minor != versionMinor+1 {
		t.Fatal("compatible future minor version not retained")
	}
}

func TestDirectLinkFutureFramesKeepReceiveDeadline(t *testing.T) {
	for _, category := range []byte{0, 1} {
		t.Run(string(rune('0'+category)), func(t *testing.T) {
			client, device := net.Pipe()
			t.Cleanup(func() { client.Close(); device.Close() })
			go func() {
				for {
					if _, err := device.Write(compatibilityFrame(category, 0, futureMessageBody())); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			start := time.Now()
			if _, err := newDirectLink(client).recv(50 * time.Millisecond); err == nil {
				t.Fatal("expected bounded receive timeout")
			}
			if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
				t.Fatalf("extension frames extended receive deadline: %s", elapsed)
			}
		})
	}
}

func TestDirectLinkStillRejectsIncompatibleFrame(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"empty known category", compatibilityFrame(0, 0, nil)},
		{"different framing version", []byte{headerMagic, headerVersion + 1, 1, 0, 0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, device := net.Pipe()
			t.Cleanup(func() { client.Close(); device.Close() })
			go func() { _, _ = device.Write(tc.frame) }()
			if _, err := newDirectLink(client).recv(time.Second); err == nil {
				t.Fatal("expected incompatible frame rejection")
			}
		})
	}
}
