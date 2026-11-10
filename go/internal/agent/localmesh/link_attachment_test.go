package localmesh

import (
	"context"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestLinkAttachmentReceiverDoesNotRequireProviderLease(t *testing.T) {
	var received []babel.LinkID
	ctx := WithLinkAttachmentReceiver(context.Background(), func(id babel.LinkID) { received = append(received, id) })
	BindConnectionLink(ctx, 7, 128, func() {})
	if len(received) != 1 || received[0] != 7 {
		t.Fatalf("operator attachment did not report its exact admitted incarnation: %v", received)
	}
}
