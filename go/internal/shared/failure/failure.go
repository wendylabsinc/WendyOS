// Package failure describes errors for clients without coupling recovery to prose.
package failure

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// These values cross the native ABI. Never renumber or reuse them.
type Code int32
type Remedy int32

const (
	Unknown                  Code = 0
	AuthenticationRequired   Code = 1
	CredentialsExpired       Code = 2
	AccountMismatch          Code = 3
	AccessDenied             Code = 4
	IdentityMismatch         Code = 5
	SecureConnectionRejected Code = 6
	NetworkUnavailable       Code = 7
	LocalNetworkDenied       Code = 8
	AgentUpdateRequired      Code = 9
	StorageFull              Code = 10
	PortInUse                Code = 11
	ServiceBusy              Code = 12
	Cancelled                Code = 13
)
const (
	None                Remedy = 0
	Retry               Remedy = 1
	SignIn              Remedy = 2
	ChooseAccount       Remedy = 3
	OpenNetworkSettings Remedy = 4
	CheckClock          Remedy = 5
	UpdateAgent         Remedy = 6
	ReviewIdentity      Remedy = 7
	FreeStorage         Remedy = 8
	ChoosePort          Remedy = 9
	Wait                Remedy = 10
)

type Info struct {
	Code   Code
	Remedy Remedy
}
type Error struct {
	Info
	Cause error
}

func (e *Error) Error() string { return e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }
func Wrap(cause error, code Code, remedy Remedy) error {
	if cause == nil {
		return nil
	}
	return &Error{Info: Info{code, remedy}, Cause: cause}
}

// Describe uses structured errors only. Diagnostic text is never an authority
// for authentication, retry, or trust decisions.
func Describe(err error) Info {
	var explicit *Error
	if errors.As(err, &explicit) {
		return explicit.Info
	}
	if errors.Is(err, context.Canceled) {
		return Info{Cancelled, None}
	}
	switch status.Code(err) {
	case codes.Unauthenticated:
		return Info{AuthenticationRequired, SignIn}
	case codes.PermissionDenied:
		return Info{AccessDenied, ChooseAccount}
	case codes.Unimplemented:
		return Info{AgentUpdateRequired, UpdateAgent}
	case codes.ResourceExhausted:
		return Info{ServiceBusy, Wait}
	case codes.Canceled:
		return Info{Cancelled, None}
	case codes.Unavailable, codes.DeadlineExceeded:
		return Info{NetworkUnavailable, Retry}
	}
	if errors.Is(err, syscall.ENOSPC) {
		return Info{StorageFull, FreeStorage}
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return Info{PortInUse, ChoosePort}
	}
	for _, cause := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ENETDOWN, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE} {
		if errors.Is(err, cause) {
			return Info{NetworkUnavailable, Retry}
		}
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return Info{NetworkUnavailable, Retry}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return Info{NetworkUnavailable, Retry}
	}
	return Info{Unknown, None}
}
