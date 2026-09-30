package commands

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestWithNextStepsKeepsTextLayoutAndExposesSteps(t *testing.T) {
	cause := errors.New("default device \"x\" is set but could not be reached: boom")
	err := withNextSteps(cause, "Confirm it with 'wendy device get-default'.", "Or connect by IP.")

	want := "default device \"x\" is set but could not be reached: boom\n" +
		"  Confirm it with 'wendy device get-default'.\n" +
		"  Or connect by IP."
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, cause) {
		t.Error("withNextSteps hid its cause from errors.Is")
	}
	wantSteps := []string{"Confirm it with 'wendy device get-default'.", "Or connect by IP."}
	if got := NextSteps(fmt.Errorf("outer: %w", err)); !reflect.DeepEqual(got, wantSteps) {
		t.Errorf("NextSteps = %q, want %q", got, wantSteps)
	}
}

func TestWithNextStepsNilAndEmpty(t *testing.T) {
	if withNextSteps(nil, "step") != nil {
		t.Error("withNextSteps(nil) must stay nil")
	}
	plain := errors.New("plain")
	if withNextSteps(plain) != plain {
		t.Error("withNextSteps without steps must return the error unchanged")
	}
	if got := NextSteps(plain); got != nil {
		t.Errorf("NextSteps(plain) = %q, want nil", got)
	}
}

func TestNextStepsFollowsJoinedErrors(t *testing.T) {
	joined := errors.Join(
		withNextSteps(errors.New("api failed"), "one"),
		withNextSteps(errors.New("worker failed"), "two"),
	)
	if got, want := NextSteps(joined), []string{"one", "two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NextSteps = %q, want %q", got, want)
	}
}

// Errors whose advice sits in their message must also expose it as steps,
// without changing the text a person sees, and StripNextSteps must recover
// the message without the steps for JSON mode.
func TestErrorsWithAdviceExposeTheirSteps(t *testing.T) {
	unknown := UnknownSubcommandError([]string{"device", "inf"})
	wantText := "unknown command \"inf\" for \"wendy device\"\n\nDid you mean this?\n\tinfo\n\nRun 'wendy device --help' to see the available commands."
	if unknown.Error() != wantText {
		t.Errorf("unknown subcommand text = %q, want the long-standing %q", unknown.Error(), wantText)
	}
	if got, want := NextSteps(unknown), []string{"Did you mean 'wendy device info'?", "Run 'wendy device --help' to see the available commands."}; !reflect.DeepEqual(got, want) {
		t.Errorf("unknown subcommand steps = %q, want %q", got, want)
	}
	if got, want := StripNextSteps(unknown.Error(), unknown), `unknown command "inf" for "wendy device"`; got != want {
		t.Errorf("StripNextSteps = %q, want %q", got, want)
	}

	tlsErr := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	if got, want := NextSteps(fmt.Errorf("connecting: %w", tlsErr)), []string{
		"Run 'wendy auth refresh-certs', then retry this command.",
		"If it still fails, rerun with WENDY_TLS_DEBUG=1 for details.",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("TLS rejection steps = %q, want %q", got, want)
	}
	if tlsErr.Error() != "TLS authentication failed. Your certificates may be outdated or incompatible with the device.\n  Run 'wendy auth refresh-certs', then retry this command.\n  If it still fails, rerun with WENDY_TLS_DEBUG=1 for details." {
		t.Errorf("TLS rejection text changed: %q", tlsErr.Error())
	}

	unauthorized := newProvisionedAgentUnauthorizedError(nil)
	if unauthorized.Error() != provisionedAgentUnauthorizedMessage || !errors.Is(unauthorized, errProvisionedAgentUnauthorized) {
		t.Errorf("unauthorized = %q, want the sentinel's text and identity", unauthorized.Error())
	}
	if got, want := NextSteps(unauthorized), []string{"Run 'wendy auth login' with an account that can access this provisioned wendy-agent."}; !reflect.DeepEqual(got, want) {
		t.Errorf("unauthorized steps = %q, want %q", got, want)
	}
	stale := newProvisionedAgentUnauthorizedError(errors.New("tls: expired certificate"))
	if got := NextSteps(stale); len(got) != 2 || !strings.Contains(got[1], "wendy auth refresh-certs") {
		t.Errorf("stale-certificate unauthorized steps = %q, want login then refresh-certs", got)
	}
}
