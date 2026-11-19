package cloudrelay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NotAdmittedError reports a session the device never joined: the caller
// waited until the grant's admission window closed. The relay cannot say why,
// so the message names the causes seen in practice.
type NotAdmittedError struct {
	Waited time.Duration
	Err    error
}

func (e *NotAdmittedError) Error() string {
	return fmt.Sprintf("the device did not accept the Cloud session within %s: it may be handling too many new connections, or its clock may be wrong (a device whose clock disagrees with Wendy Cloud's refuses every session)", e.Waited.Round(time.Second))
}

func (e *NotAdmittedError) Unwrap() error { return e.Err }

// admissionError turns a join that ended because the grant's admission window
// closed into a NotAdmittedError. The caller's own cancellation, and a join
// that failed while the window was still open, are returned unchanged.
func admissionError(ctx context.Context, c claims, started time.Time, err error) error {
	if ctx.Err() != nil || time.Now().Before(time.Unix(c.Exp, 0).Add(-cloudClockSkew)) {
		return err
	}
	switch status.Code(err) {
	// The caller's admission timer, or the broker closing admission first.
	case codes.Canceled, codes.DeadlineExceeded, codes.FailedPrecondition:
	default:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return &NotAdmittedError{Waited: time.Since(started), Err: err}
}
