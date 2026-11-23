package resetcleanup

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRefusalCarriesTheCauseAcrossTheWire(t *testing.T) {
	cause := errors.New("wendy-net policy rule ownership conflict during reset cleanup: family=2 priority=51820 table=51820")
	err := Refusal(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying", cause)

	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
	// What a client that knows nothing of the detail shows its user.
	message := status.Convert(err).Message()
	if !strings.HasPrefix(message, "reset resource cleanup incomplete; agent recovery is retrying: ") || !strings.Contains(message, "priority=51820") {
		t.Fatalf("message = %q, want the stable text followed by the cause", message)
	}

	// The status survives the trip a real RPC makes: to its wire form and back.
	received := status.FromProto(status.Convert(err).Proto()).Err()
	got, ok := Cause(received)
	if !ok || got != cause.Error() {
		t.Fatalf("Cause() = %q, %v; want %q, true", got, ok, cause.Error())
	}
	// A caller that added context with %w still recognises it.
	if _, ok := Cause(fmt.Errorf("checking enrollment: %w", received)); !ok {
		t.Fatal("Cause() did not recognise a wrapped refusal")
	}
}

func TestCauseIgnoresOtherErrors(t *testing.T) {
	for name, err := range map[string]error{
		"nil":                nil,
		"plain":              errors.New("reset resource cleanup incomplete"),
		"same code, no info": status.Error(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying"),
		"agent down":         status.Error(codes.Unavailable, "connection refused"),
	} {
		if cause, ok := Cause(err); ok {
			t.Errorf("%s: Cause() = %q, true; want not recognised", name, cause)
		}
	}
}

// Cleanup errors are joined from several subsystems, one per line. The cause
// travels in a status message and in gRPC trailers, so it is one bounded line.
func TestRefusalFlattensAndBoundsTheCause(t *testing.T) {
	joined := errors.Join(errors.New("sharing: nft flush failed"), errors.New("  interface \"wendy0\" is not owned by wendy-net  "), errors.New(""))
	got, ok := Cause(Refusal(codes.FailedPrecondition, "reset resource cleanup must finish before enrollment", joined))
	if !ok || got != `sharing: nft flush failed; interface "wendy0" is not owned by wendy-net` {
		t.Fatalf("Cause() = %q, %v; want the lines joined with \"; \"", got, ok)
	}

	long := errors.New(strings.Repeat("é", 4000))
	got, ok = Cause(Refusal(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying", long))
	if !ok || len(got) > maxCauseBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("Cause() returned %d bytes (valid UTF-8: %v); want at most %d, cut on a character and marked", len(got), utf8.ValidString(got), maxCauseBytes)
	}
}

func TestRefusalWithoutACauseStillIdentifiesItself(t *testing.T) {
	err := Refusal(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying", nil)
	if message := status.Convert(err).Message(); message != "reset resource cleanup incomplete; agent recovery is retrying" {
		t.Fatalf("message = %q, want the stable text alone", message)
	}
	if cause, ok := Cause(err); !ok || cause != "" {
		t.Fatalf("Cause() = %q, %v; want \"\", true", cause, ok)
	}
}
