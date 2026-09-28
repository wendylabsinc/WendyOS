package commands

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deviceDialError records the address a failed device connection was aimed
// at. It never changes the message: the connect ladder's retry predicates
// (clock skew, handshake timeout, cert refresh) read the text. The CLI entry
// point reads DeviceAddress to name the target when it rewrites the transport
// error for display.
type deviceDialError struct {
	addr  string
	cause error
}

// newDeviceDialError wraps cause with the address that was dialled; nil stays nil.
func newDeviceDialError(addr string, cause error) error {
	if cause == nil {
		return nil
	}
	return &deviceDialError{addr: addr, cause: cause}
}

func (e *deviceDialError) Error() string { return e.cause.Error() }
func (e *deviceDialError) Unwrap() error { return e.cause }

// DeviceAddress is the host:port the failed connection was aimed at.
func (e *deviceDialError) DeviceAddress() string { return e.addr }

// Is classifies a transport failure (gRPC Unavailable: refused, reset, no
// route) as errDeviceUnreachable. ErrorClass checks the more specific
// verdicts that also arrive as Unavailable (TLS rejection, missing
// credentials, identity refusal) before device_unreachable, so they win.
func (e *deviceDialError) Is(target error) bool {
	return target == errDeviceUnreachable && status.Code(e.cause) == codes.Unavailable
}
