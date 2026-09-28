package commands

import (
	"errors"
	"fmt"
	"reflect"
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
