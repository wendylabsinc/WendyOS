//go:build linux

package localmesh

import "testing"

func TestDirectoryCertificateReplyRetainedBehindAdmittedGossip(t *testing.T) {
	n := &Node{}
	p := &nodePeer{control: make(chan ControlMessage, 1)}
	admitted := ControlMessage{Kind: "identity-request", Principal: "admitted"}
	reply := ControlMessage{Kind: "identity-request", Principal: "requested"}
	p.control <- admitted
	if !n.queueReplies(p, []ControlMessage{reply}) || len(p.replies) != 1 {
		t.Fatal("requested protocol response discarded under backpressure")
	}
	if got := <-p.control; got.Principal != admitted.Principal {
		t.Fatal("already queued gossip was replaced")
	}
	n.queue(p, []ControlMessage{{Kind: "identity-request", Principal: "later gossip"}})
	if got := <-p.control; got.Principal != reply.Principal || len(p.replies) != 0 || p.replyBytes != 0 {
		t.Fatal("requested reply did not drain ahead of new gossip")
	}
	p.control <- admitted
	for range maxDirectoryReplyFrames {
		if !n.queueReplies(p, []ControlMessage{reply}) {
			t.Fatal("reply reserve filled too early")
		}
	}
	bytes := p.replyBytes
	if n.queueReplies(p, []ControlMessage{reply}) || len(p.replies) != maxDirectoryReplyFrames || bytes != p.replyBytes {
		t.Fatal("overload exceeded or replaced the retained response reserve")
	}
}
