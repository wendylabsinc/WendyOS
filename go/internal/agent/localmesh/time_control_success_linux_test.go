//go:build linux

package localmesh

import (
	"bytes"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestNormalTimestampCompletionDispatchAndPull(t *testing.T) {
	data := bytes.Repeat([]byte("nonce-bound timestamp"), 125)
	p := &nodePeer{id: 7, control: make(chan ControlMessage, 1), timestamp: &timestampReply{id: 42, expires: time.Now().Add(time.Minute)}}
	n := &Node{peers: map[babel.LinkID]*nodePeer{p.id: p}}
	if err := n.handle(nodeEvent{id: p.id, timestamp: &timestampReply{id: 42, data: data, expires: p.timestamp.expires, pending: true}}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		message := <-p.control
		if message.Kind != "time" || message.Time.Op != "reply" || int(message.Time.Offset) != len(got) || int(message.Time.Total) != len(data) {
			t.Fatalf("unexpected reply: %+v", message)
		}
		got = append(got, message.Time.Data...)
		if len(got) == len(data) {
			break
		}
		if err := n.handleControl(p, &ControlMessage{Kind: "time", Time: &TimestampControl{ID: 42, Op: "next", Offset: uint32(len(got))}}); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, data) || p.timestamp != nil {
		t.Fatal("timestamp transfer did not finish exactly")
	}
}

func TestNormalTimestampPumpResumesAfterQueueDrain(t *testing.T) {
	p := &nodePeer{control: make(chan ControlMessage, 1), timestamp: &timestampReply{id: 42, data: []byte("signed reply"), expires: time.Now().Add(time.Minute), pending: true}}
	n := &Node{}
	p.control <- ControlMessage{Kind: "hello"}
	// A full ordinary reply reserve delays the pending timestamp without
	// advancing its offset. The existing maintenance pump retries it later.
	for range maxDirectoryReplyFrames {
		p.replies = append(p.replies, ControlMessage{Kind: "hello"})
	}
	n.pumpTimestamp(p)
	if p.timestamp == nil || p.timestamp.offset != 0 || !p.timestamp.pending {
		t.Fatal("queued timestamp advanced before admission")
	}
	<-p.control
	p.replies = nil
	p.replyBytes = 0
	n.pumpTimestamp(p)
	if got := <-p.control; got.Kind != "time" || string(got.Time.Data) != "signed reply" || p.timestamp != nil {
		t.Fatal("maintenance pump lost timestamp reply")
	}
}
