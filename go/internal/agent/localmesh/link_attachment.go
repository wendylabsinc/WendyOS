package localmesh

import (
	"context"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type linkAttachmentReceiverKey struct{}

// WithLinkAttachmentReceiver reports the exact local Node incarnation after
// actor admission. The callback runs on the attachment owner, outside the actor.
// It grants neither peer identity nor routing authority.
func WithLinkAttachmentReceiver(ctx context.Context, receive func(babel.LinkID)) context.Context {
	return context.WithValue(ctx, linkAttachmentReceiverKey{}, receive)
}

func reportLinkAttachment(ctx context.Context, id babel.LinkID) {
	if receive, _ := ctx.Value(linkAttachmentReceiverKey{}).(func(babel.LinkID)); receive != nil {
		receive(id)
	}
}
