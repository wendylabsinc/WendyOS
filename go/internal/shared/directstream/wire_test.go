package directstream

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestFlowWireGolden(t *testing.T) {
	cases := []struct {
		f   FlowFrame
		hex string
	}{
		{FlowFrame{Kind: FrameData, AttachmentID: 0x11223344, FlowID: 1, Payload: []byte{0xaa, 0xbb}}, "011122334400000001aabb"},
		{FlowFrame{Kind: FrameState, AttachmentID: 0x11223344, StateCount: 1, States: [4]StateEntry{{FlowID: 1, Received: 258, Limit: 1024, FINSeen: true}}}, "021122334401000000010000000000000102000000000000040001"},
		{FlowFrame{Kind: FrameFIN, AttachmentID: 1, FlowID: 2, Value: 42}, "030000000100000002000000000000002a"},
		{FlowFrame{Kind: FrameOpen, AttachmentID: 1, FlowID: 1, Service: ServiceUDP, Port: 42, DatagramSize: 4096}, "04000000010000000102002a1000"},
		{FlowFrame{Kind: FrameOpenResult, AttachmentID: 1, FlowID: 1, Code: 2}, "0500000001000000010002"},
		{FlowFrame{Kind: FrameOpenCredit, AttachmentID: 1, Value: 10}, "0600000001000000000000000a"},
		{FlowFrame{Kind: FrameDatagram, AttachmentID: 1, FlowID: 1, Value: 2, DatagramSize: 4, FragmentOffset: 2, Payload: []byte{0xaa, 0xbb}}, "070000000100000001000000000000000200040002aabb"},
		{FlowFrame{Kind: FrameReset, AttachmentID: 1, FlowID: 1, Code: 3}, "0800000001000000010003"},
		{FlowFrame{Kind: FrameCloseACK, AttachmentID: 1, FlowID: 1}, "090000000100000001"},
		{FlowFrame{Kind: FrameDatagramLimit, AttachmentID: 1, DatagramSize: 4096}, "0a000000011000"},
		{FlowFrame{Kind: FrameDatagramLimit, AttachmentID: 1}, "0a000000010000"},
	}
	for _, c := range cases {
		want, _ := hex.DecodeString(c.hex)
		buf := make([]byte, MaxFlowFrame)
		n, err := c.f.Encode(buf)
		if err != nil || !bytes.Equal(buf[:n], want) {
			t.Fatalf("encode %d: %x %v", c.f.Kind, buf[:n], err)
		}
		f, err := DecodeFlowFrame(want)
		if err != nil {
			t.Fatal(err)
		}
		n, err = f.Encode(buf)
		if err != nil || !bytes.Equal(buf[:n], want) {
			t.Fatal("round trip", err)
		}
	}
}
