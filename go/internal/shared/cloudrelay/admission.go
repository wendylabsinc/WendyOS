package cloudrelay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errAdmissionTimeout = errors.New("Cloud session admission timer expired")

// NotAdmittedError reports that the admission window closed before the caller
// connected. The closing window does not identify which endpoint was delayed.
type NotAdmittedError struct {
	Waited time.Duration
	Err    error
}

func (e *NotAdmittedError) Error() string {
	return fmt.Sprintf("Cloud session admission did not complete within %s", e.Waited.Round(100*time.Millisecond))
}

func (e *NotAdmittedError) Unwrap() error { return e.Err }

// admissionError distinguishes a locally expired admission timer and the
// broker's explicit admission-close result from other join failures.
func admissionError(parent context.Context, cause error, started time.Time, err error) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	canceled := errors.Is(err, context.Canceled)
	var rpcError interface{ GRPCStatus() *status.Status }
	if errors.As(err, &rpcError) {
		if st := rpcError.GRPCStatus(); st != nil {
			if st.Code() == codes.FailedPrecondition && st.Message() == "admission closed" {
				return &NotAdmittedError{Waited: time.Since(started), Err: err}
			}
			canceled = canceled || (st.Code() == codes.Canceled && st.Message() == context.Canceled.Error())
		}
	}
	if cause == errAdmissionTimeout && canceled {
		return &NotAdmittedError{Waited: time.Since(started), Err: err}
	}
	return err
}
