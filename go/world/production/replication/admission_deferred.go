package replication

import (
	"context"
	"errors"
)

// ErrAdmissionDeferred is the reason-neutral outcome of a remote Deferred
// reply. The existing wire does not identify the receiver's local reason.
var ErrAdmissionDeferred = errors.New("replica admission deferred")

type admissionNotStarted struct{ cause error }

func (e *admissionNotStarted) Error() string {
	return "replica admission not started: " + e.cause.Error()
}
func (e *admissionNotStarted) Unwrap() error { return e.cause }

// AdmissionNotStarted is for a trusted Store whose host guard returned an error
// without invoking its admission callback. No record, lease or floor admission
// ran. The guard may have changed other metadata. This is not a success or a
// grant, and must never label an entered callback, uncertain commit or fault.
func AdmissionNotStarted(cause error) error {
	if cause == nil || errors.Is(cause, ErrUncertain) {
		return errors.Join(ErrUncertain, cause)
	}
	return &admissionNotStarted{cause: cause}
}

// IsAdmissionNotStarted accepts only the direct typed outcome. Wrapping/joining
// it with another failure cannot turn that failure into a known deferral.
func IsAdmissionNotStarted(err error) bool {
	e, ok := err.(*admissionNotStarted)
	return ok && e.cause != nil && !errors.Is(e.cause, ErrUncertain)
}

func admissionDeferred(ctx context.Context, item Item, err error) bool {
	if IsAdmissionNotStarted(err) {
		// Cancellation while waiting for the host guard does not turn a known
		// unentered admission into an uncertain write. No work is retried here.
		return true
	}
	return item.Entry.Kind == Record && errors.Is(err, ErrRecordWindowDeferred) &&
		!errors.Is(err, ErrUncertain) && ctx.Err() == nil
}
