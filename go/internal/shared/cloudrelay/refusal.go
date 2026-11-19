package cloudrelay

import (
	"errors"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RefusedError reports a session the device refused, with the reason it gave.
type RefusedError struct{ Reason pb.SessionRefusalReason }

func (e *RefusedError) Error() string {
	switch e.Reason {
	case pb.SessionRefusalReason_SESSION_REFUSAL_REASON_CLOCK:
		return "the device refused the Cloud session because its clock disagrees with Wendy Cloud's; it refuses every session until its clock is right, which happens once it can reach a time server"
	case pb.SessionRefusalReason_SESSION_REFUSAL_REASON_BUSY:
		return "the device refused the Cloud session because it is handling too many connections; try again in a moment"
	case pb.SessionRefusalReason_SESSION_REFUSAL_REASON_SERVICE_UNREACHABLE:
		return "the device refused the Cloud session because nothing answered on that port on the device (the tunnel connects to 127.0.0.1 there)"
	case pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSUPPORTED:
		return "the device refused the Cloud session because its agent does not serve that service; the agent may need an update"
	}
	return "the device refused the Cloud session"
}

// serviceUnreachableError: nothing accepted the agent's connection to the dial
// target of an offer it had acknowledged.
type serviceUnreachableError struct{ err error }

func (e *serviceUnreachableError) Error() string { return e.err.Error() }
func (e *serviceUnreachableError) Unwrap() error { return e.err }

// refusalReason is the reason the agent gives the caller for an offer it will
// not serve. UNSPECIFIED means it says nothing, as before: an offer that fails
// verification may not be Cloud's, and the caller of one that expired has gone.
func refusalReason(err error) pb.SessionRefusalReason {
	var unreachable *serviceUnreachableError
	switch {
	case errors.Is(err, errArtifactValidity):
		return pb.SessionRefusalReason_SESSION_REFUSAL_REASON_CLOCK
	case status.Code(err) == codes.ResourceExhausted:
		return pb.SessionRefusalReason_SESSION_REFUSAL_REASON_BUSY
	case errors.As(err, &unreachable):
		return pb.SessionRefusalReason_SESSION_REFUSAL_REASON_SERVICE_UNREACHABLE
	case errors.Is(err, errUnsupportedService):
		return pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSUPPORTED
	}
	return pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSPECIFIED
}
