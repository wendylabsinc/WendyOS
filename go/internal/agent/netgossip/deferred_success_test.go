package netgossip

import (
	"bytes"
	"testing"
	"time"
)

func TestDeferredUploadWaitsForCreditAndTransfersExactBody(t *testing.T) {
	now := time.Now()
	source := NewPool().NewSession(Manifest)
	receiver := NewPool().NewSession(Manifest)
	defer source.Close()
	defer receiver.Close()
	body := bytes.Repeat([]byte("ordinary deferred publication"), 150)
	have := source.Offer(body, now.Add(time.Minute), now, nil)
	want, _, err := receiver.Receive(*have, now)
	if err != nil || want == nil {
		t.Fatal("missing ordinary WANT", err)
	}
	reply, _, err := source.ReceiveDeferred(*want, now)
	if err != nil || reply != nil {
		t.Fatal("upload advanced before DATA credit", err)
	}
	if frames := source.Poll(now, 0); len(frames) != 0 {
		t.Fatal("zero-credit poll advanced upload")
	}
	var received []byte
	for turns := 0; turns < 20 && len(received) == 0; turns++ {
		frames := source.Poll(now, 1)
		if len(frames) != 1 {
			t.Fatal("one-credit upload did not make progress")
		}
		_, complete, err := receiver.Receive(*frames[0], now)
		if err != nil {
			t.Fatal(err)
		}
		if len(complete) > 0 {
			received = complete
		}
	}
	if !bytes.Equal(received, body) {
		t.Fatal("deferred upload lost or duplicated body")
	}
}
