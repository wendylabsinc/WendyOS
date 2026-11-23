// Package resetcleanup identifies, across the agent/client boundary, an agent
// that was reset but has not finished removing what its previous enrollment
// left on the host.
//
// Such an agent is unenrolled and serves its setup port, but it refuses to
// report its enrollment state or to enroll until the cleanup succeeds. The
// refusal keeps the gRPC code it always had and adds what is failing, so a
// person can act on it and a client can tell this state from an agent that is
// simply not answering.
package resetcleanup

import (
	"errors"
	"strings"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// Domain and Incomplete name the refusal in a google.rpc.ErrorInfo detail.
	Domain     = "wendy.dev"
	Incomplete = "RESET_CLEANUP_INCOMPLETE"

	causeKey = "cause"
	// maxCauseBytes bounds the cause, which travels in the status message and
	// again in the gRPC trailers.
	maxCauseBytes = 1024
)

// Refusal is the status an agent returns while reset cleanup is incomplete.
// message is the stable text clients have always seen; the cause is appended
// to it for a person and attached as a detail for a client.
func Refusal(code codes.Code, message string, cause error) error {
	text := causeText(cause)
	if text != "" {
		message += ": " + text
	}
	st := status.New(code, message)
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Domain:   Domain,
		Reason:   Incomplete,
		Metadata: map[string]string{causeKey: text},
	})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

// Cause reports whether err is that refusal, wrapped or not, and what the
// agent said was failing. The cause is empty when the agent gave none.
func Cause(err error) (cause string, ok bool) {
	var carrier interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &carrier) {
		return "", false
	}
	st := carrier.GRPCStatus()
	if st == nil {
		return "", false
	}
	for _, detail := range st.Details() {
		if info, isInfo := detail.(*errdetails.ErrorInfo); isInfo && info.GetDomain() == Domain && info.GetReason() == Incomplete {
			return info.GetMetadata()[causeKey], true
		}
	}
	return "", false
}

// causeText renders a cleanup error as one bounded line. Cleanup errors are
// joined from several subsystems, one per line.
func causeText(cause error) string {
	if cause == nil {
		return ""
	}
	var parts []string
	for _, line := range strings.Split(cause.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	text := strings.Join(parts, "; ")
	if len(text) <= maxCauseBytes {
		return text
	}
	const ellipsis = "…"
	cut := maxCauseBytes - len(ellipsis)
	// Never cut inside a multi-byte character.
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + ellipsis
}
