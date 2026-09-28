package commands

import (
	"context"
	"errors"
	"net"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errDeviceNotResolved classifies a device host name that resolved to no
// address. Unlike an unreachable device, retrying the same name cannot help
// until the name or DNS changes.
var errDeviceNotResolved = errors.New("device host not resolved")

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

// Is classifies the failure by what it says about the device. A host name
// that resolves to nothing is errDeviceNotResolved, except an mDNS (.local)
// name, which only resolves while its device is on the network. Any other
// transport failure (gRPC Unavailable: refused, reset, no route) and a dial
// that timed out are errDeviceUnreachable. ErrorClass checks the more
// specific verdicts that also arrive this way (TLS rejection, missing
// credentials, identity refusal) first, so they win.
func (e *deviceDialError) Is(target error) bool {
	notResolved := isResolverMiss(e.cause)
	switch target {
	case errDeviceNotResolved:
		return notResolved && !isMDNSAddress(e.addr)
	case errDeviceUnreachable:
		if notResolved {
			return isMDNSAddress(e.addr)
		}
		code := status.Code(e.cause)
		return code == codes.Unavailable || code == codes.DeadlineExceeded ||
			errors.Is(e.cause, context.DeadlineExceeded)
	}
	return false
}

// isResolverMiss reports whether a dial failed because the host name
// resolved to no address: gRPC's resolver reports "produced zero addresses",
// and the Go resolver "no such host".
func isResolverMiss(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "produced zero addresses") || strings.Contains(msg, "no such host")
}

// isMDNSAddress reports whether addr's host is an mDNS (.local) name.
func isMDNSAddress(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), ".local")
}

// DeviceDialErrorClass classifies an error from a failed device connection,
// one that records the address it dialled, or returns "" when err is not
// one. The CLI entry point checks it before its generic deadline classes: a
// dial that timed out means the device did not answer, which is what the
// caller needs to know.
func DeviceDialErrorClass(err error) string {
	var dial interface{ DeviceAddress() string }
	if !errors.As(err, &dial) {
		return ""
	}
	return ErrorClass(&deviceDialError{addr: dial.DeviceAddress(), cause: err})
}
