package localmesh

import (
	"context"
	"errors"
	"time"
)

// receiveDatagram uses a deadline only while incomplete IP buffers exist. It
// expires them even on an otherwise idle link, without closing the carrier or
// starting a goroutine per packet. The same owner performs every map mutation.
func (a *IPAssembler) receiveDatagram(ctx context.Context, receive func(context.Context) ([]byte, error)) ([]byte, error) {
	for {
		a.Expire(time.Now())
		next := a.nextExpiry()
		if next.IsZero() {
			return receive(ctx)
		}
		receiveCtx, cancel := context.WithDeadline(ctx, next)
		d, err := receive(receiveCtx)
		expired := errors.Is(err, context.DeadlineExceeded) && receiveCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if expired {
			continue
		}
		return d, err
	}
}
