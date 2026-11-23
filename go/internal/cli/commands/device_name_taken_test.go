package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// stubOrgDeviceNames makes the organization's device list names, or err.
func stubOrgDeviceNames(t *testing.T, names []string, err error) *int {
	t.Helper()
	orig := orgDeviceNamesFn
	t.Cleanup(func() { orgDeviceNamesFn = orig })
	calls := 0
	orgDeviceNamesFn = func(context.Context, *config.AuthConfig) ([]string, error) {
		calls++
		return names, err
	}
	return &calls
}

func stubPromptDeviceNameDefault(t *testing.T, fn func(prompt, hint, def string, validate tui.ValidateFunc) (string, error)) {
	t.Helper()
	orig := promptDeviceNameDefault
	t.Cleanup(func() { promptDeviceNameDefault = orig })
	promptDeviceNameDefault = fn
}

func takenErr(name string) error {
	return fmt.Errorf("Cloud refused this enrollment before minting anything: %w: another device is already named %s", cloudenroll.ErrDeviceNameTaken, name)
}

func TestTakenNameMessage(t *testing.T) {
	if got, want := takenNameMessage("brave-dolphin", ""), "Another device in your organization already uses brave-dolphin."; got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
	if got, want := takenNameMessage("brave-dolphin", "Wendy Labs, Inc."), "Another device in Wendy Labs, Inc. already uses brave-dolphin."; got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestSuggestFreeName(t *testing.T) {
	takenSet := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, m := range names {
				if m == n {
					return true
				}
			}
			return false
		}
	}
	if got := suggestFreeName("brave-dolphin", 55, takenSet("brave-dolphin-2")); got != "brave-dolphin-3" {
		t.Fatalf("got %q; want brave-dolphin-3", got)
	}
	if got := suggestFreeName("brave-dolphin", 14, takenSet()); got != "" {
		t.Fatalf("got %q; want no suggestion past the length limit", got)
	}
	all := []string{}
	for i := 2; i <= 9; i++ {
		all = append(all, fmt.Sprintf("brave-dolphin-%d", i))
	}
	if got := suggestFreeName("brave-dolphin", 55, takenSet(all...)); got != "" {
		t.Fatalf("got %q; want no suggestion when 2…9 are taken", got)
	}
}

func TestNameCheckerIgnoresCase(t *testing.T) {
	calls := stubOrgDeviceNames(t, []string{"Brave-Dolphin"}, nil)
	taken := newNameChecker(context.Background(), &config.AuthConfig{})
	if !taken("brave-dolphin") || taken("otter") {
		t.Fatal("want brave-dolphin taken regardless of case, otter free")
	}
	if *calls != 1 {
		t.Fatalf("listed the organization's devices %d times; want once", *calls)
	}
}

func TestNameCheckerLookupFailureIsUnknown(t *testing.T) {
	stubOrgDeviceNames(t, nil, errors.New("issuer not found"))
	if newNameChecker(context.Background(), &config.AuthConfig{})("brave-dolphin") {
		t.Fatal("a failed lookup must report the name as free")
	}
}

func TestOrgDeviceNamesNoCertificates(t *testing.T) {
	if _, err := orgDeviceNamesFn(context.Background(), &config.AuthConfig{}); err == nil {
		t.Fatal("want an error for a session without certificates")
	}
}

func TestOrgDeviceNamesHasDeadline(t *testing.T) {
	orig := fetchOrgAssetsFn
	t.Cleanup(func() { fetchOrgAssetsFn = orig })
	var deadline time.Time
	var hasDeadline bool
	fetchOrgAssetsFn = func(ctx context.Context, _ *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
		deadline, hasDeadline = ctx.Deadline()
		return []*cloudpbv2.Asset{{Name: "brave-dolphin"}}, nil
	}
	names, err := orgDeviceNamesFn(context.Background(), &config.AuthConfig{Certificates: []config.CertificateInfo{{}}})
	if err != nil || len(names) != 1 || names[0] != "brave-dolphin" {
		t.Fatalf("names %v, err %v", names, err)
	}
	if !hasDeadline || time.Until(deadline) > 10*time.Second {
		t.Fatalf("lookup deadline %v (set %v); want at most 10s", deadline, hasDeadline)
	}
}

func TestRetryTakenDeviceNameNoTerminal(t *testing.T) {
	name := "brave-dolphin"
	err := retryTakenDeviceName(&name, nameRetry{noTTYHint: noTTYHintInstall, validate: validateDeviceName, maxLen: maxDeviceNameLen},
		func(n string) error { return takenErr(n) })
	want := "Another device in your organization already uses brave-dolphin. Pass a different --device-name."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v; want %q", err, want)
	}
}

func TestRetryTakenDeviceNameSuggestsAndRetries(t *testing.T) {
	stubPromptDeviceNameDefault(t, func(_, hint, def string, _ tui.ValidateFunc) (string, error) {
		if def != "brave-dolphin-2" || hint != "(Ctrl+C to stop)" {
			t.Fatalf("suggested %q with hint %q; want brave-dolphin-2 and how to stop", def, hint)
		}
		return def, nil
	})
	name := "brave-dolphin"
	var tried []string
	var err error
	out := captureStdout(t, func() {
		err = retryTakenDeviceName(&name, nameRetry{interactive: true, noTTYHint: noTTYHintInstall, validate: validateDeviceName, maxLen: maxDeviceNameLen},
			func(n string) error {
				tried = append(tried, n)
				if n == "brave-dolphin" {
					return takenErr(n)
				}
				return nil
			})
	})
	if err != nil || name != "brave-dolphin-2" || strings.Join(tried, ",") != "brave-dolphin,brave-dolphin-2" {
		t.Fatalf("err %v, name %q, tried %v", err, name, tried)
	}
	if !strings.Contains(out, "Another device in your organization already uses brave-dolphin.") {
		t.Fatalf("output %q; want the taken message", out)
	}
}

func TestRetryTakenDeviceNameCtrlCStops(t *testing.T) {
	stubPromptDeviceNameDefault(t, func(string, string, string, tui.ValidateFunc) (string, error) { return "", tui.ErrCancelled })
	name := "brave-dolphin"
	var err error
	captureStdout(t, func() {
		err = retryTakenDeviceName(&name, nameRetry{interactive: true, noTTYHint: noTTYHintInstall, validate: validateDeviceName, maxLen: maxDeviceNameLen},
			func(n string) error { return takenErr(n) })
	})
	if !errors.Is(err, tui.ErrCancelled) {
		t.Fatalf("got %v; want tui.ErrCancelled", err)
	}
}

func TestRetryTakenDeviceNameNamesTheOrganization(t *testing.T) {
	name := "brave-dolphin"
	err := retryTakenDeviceName(&name, nameRetry{noTTYHint: noTTYHintInstall, validate: validateDeviceName, maxLen: maxDeviceNameLen, org: "Wendy Labs, Inc."},
		func(n string) error { return takenErr(n) })
	want := "Another device in Wendy Labs, Inc. already uses brave-dolphin. Pass a different --device-name."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v; want %q", err, want)
	}
}

// Rename learns the organization only inside the call Cloud refused.
func TestRetryTakenDeviceNameUsesTheRefusingOrganization(t *testing.T) {
	name := "living-room"
	err := retryTakenDeviceName(&name, nameRetry{noTTYHint: noTTYHintEnroll, validate: validateHostnameArg, maxLen: maxHostnameLen},
		func(n string) error { return &takenInOrgError{org: "Benjamin Lab", err: takenErr(n)} })
	want := "Another device in Benjamin Lab already uses living-room. Pass a different --name."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v; want %q", err, want)
	}
}

// An organization's name is its admins' free text; it must not reach the terminal raw.
func TestTakenNameMessageStripsControlFromOrganization(t *testing.T) {
	if got := takenNameMessage("brave-dolphin", "Evil\x1b[2J\nOrg"); strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("got %q; want control characters removed", got)
	}
	if got, want := takenNameMessage("brave-dolphin", "\x1b\n"), "Another device in your organization already uses brave-dolphin."; got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}
