package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/resetcleanup"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The Agent's refusal after Cloud deleted the asset (mesh-1, 2026-10-26).
var errAgentRefusedReset = status.Error(codes.FailedPrecondition, "independently verified Cloud reset authorization required; keys retained")

type resetRetryHarness struct {
	order  []string
	tokens []string // authorization each reset attempt carried
	waits  []time.Duration
	auths  int
}

func (h *resetRetryHarness) ops(t *testing.T, authorize func(n int) (*v2AuthorizedReset, error), reset func(n int) error, wait func() error) v2UnenrollOps {
	return v2UnenrollOps{
		lookup: func(context.Context, string) (*v2AssetState, error) { return testLifecycle(false), nil },
		authorize: func(context.Context, string) (*v2AuthorizedReset, error) {
			h.auths++
			h.order = append(h.order, "authorize")
			if authorize != nil {
				if a, err := authorize(h.auths); a != nil || err != nil {
					return a, err
				}
			}
			a := testAuthorizedReset(t)
			a.token = fmt.Appendf(nil, "authorization-%d", h.auths)
			return a, nil
		},
		proof: func(context.Context) ([]byte, error) {
			h.order = append(h.order, "verify-PKI")
			return []byte("verified"), nil
		},
		reset: func(_ context.Context, _, _ string, _ []byte, a *v2AuthorizedReset) error {
			h.order = append(h.order, "reset")
			h.tokens = append(h.tokens, string(a.token))
			return reset(len(h.tokens))
		},
		wait: func(_ context.Context, d time.Duration) error {
			h.order = append(h.order, "wait")
			h.waits = append(h.waits, d)
			if wait != nil {
				return wait()
			}
			return nil
		},
	}
}

// Cloud has already deleted the asset when the device verifies its reset. A
// momentary refusal must be retried with a new signed authorization instead of
// leaving a device Cloud no longer knows still holding its keys.
func TestV2UnenrollRetriesRefusedResetWithFreshAuthorization(t *testing.T) {
	var h resetRetryHarness
	ops := h.ops(t, nil, func(n int) error {
		if n == 1 {
			return errAgentRefusedReset
		}
		return nil
	}, nil)
	j := testUnenrollProgress()
	if err := performV2Unenroll(context.Background(), &j, ops); err != nil {
		t.Fatalf("one momentary device refusal ended the unenroll: %v", err)
	}
	if !j.Deleted || !j.Revoked || !j.Reset {
		t.Fatalf("progress = %+v, want deleted, revoked and reset", j)
	}
	if got := strings.Join(h.order, ","); got != "authorize,verify-PKI,authorize,reset,wait,authorize,reset" {
		t.Fatalf("order = %s", got)
	}
	// Each attempt carries its own Cloud authorization, never a reused one.
	if !slices.Equal(h.tokens, []string{"authorization-2", "authorization-3"}) {
		t.Fatalf("reset authorizations = %v, want a fresh one per attempt", h.tokens)
	}
	if !slices.Equal(h.waits, v2ResetRetryDelays[:1]) {
		t.Fatalf("waits = %v", h.waits)
	}
}

func TestV2UnenrollResetRetryIsBoundedAndNarrow(t *testing.T) {
	changed := func() *v2AuthorizedReset {
		d := testLifecycle(true).Deleted
		d.DeletedAt = timestamppb.New(time.Unix(1700000001, 0))
		raw, _ := proto.Marshal(d)
		return &v2AuthorizedReset{deletion: raw, token: []byte("other")}
	}
	for name, tc := range map[string]struct {
		reset      func(int) error
		authorize  func(int) (*v2AuthorizedReset, error)
		wait       func() error
		wantResets int
		wantErr    string
	}{
		// Refusals keep failing: stop after the bounded attempts.
		"always-refused": {reset: func(int) error { return errAgentRefusedReset }, wantResets: 1 + len(v2ResetRetryDelays), wantErr: "local reset unconfirmed after 3 attempt(s)"},
		// A transport error may hide a reset that completed; never repeat it.
		"transport": {reset: func(int) error { return status.Error(codes.Unavailable, "connection reset") }, wantResets: 1, wantErr: "local reset unconfirmed"},
		"deadline":  {reset: func(int) error { return status.Error(codes.DeadlineExceeded, "deadline") }, wantResets: 1, wantErr: "local reset unconfirmed"},
		// A reply the CLI cannot accept is not an Agent refusal either.
		"bad-receipt": {reset: func(int) error { return errors.New("local completion evidence mismatch") }, wantResets: 1, wantErr: "local reset unconfirmed"},
		"permission":  {reset: func(int) error { return status.Error(codes.PermissionDenied, "operator") }, wantResets: 1, wantErr: "local reset unconfirmed"},
		// A retry still needs Cloud to re-sign the same deletion.
		"refresh-fails": {
			reset: func(int) error { return errAgentRefusedReset },
			authorize: func(n int) (*v2AuthorizedReset, error) {
				if n == 3 {
					return nil, errors.New("cloud unavailable")
				}
				return nil, nil
			},
			wantResets: 1, wantErr: "refreshing Cloud reset authorization",
		},
		"evidence-changed": {
			reset: func(int) error { return errAgentRefusedReset },
			authorize: func(n int) (*v2AuthorizedReset, error) {
				if n == 3 {
					return changed(), nil
				}
				return nil, nil
			},
			wantResets: 1, wantErr: "Cloud deletion evidence changed",
		},
		"canceled": {reset: func(int) error { return errAgentRefusedReset }, wait: func() error { return context.Canceled }, wantResets: 1, wantErr: "retry interrupted"},
	} {
		t.Run(name, func(t *testing.T) {
			var h resetRetryHarness
			j := testUnenrollProgress()
			err := performV2Unenroll(context.Background(), &j, h.ops(t, tc.authorize, tc.reset, tc.wait))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if len(h.tokens) != tc.wantResets || j.Reset || !j.Deleted {
				t.Fatalf("resets = %d (want %d), progress %+v", len(h.tokens), tc.wantResets, j)
			}
			if name == "always-refused" && !slices.Equal(h.waits, v2ResetRetryDelays) {
				t.Fatalf("waits = %v, want %v", h.waits, v2ResetRetryDelays)
			}
			if name == "always-refused" && status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("the Agent's refusal code was lost: %v", err)
			}
		})
	}
}

// A device left unreset after the Cloud deletion must come with the exact
// command that finishes the job, not just the refusal.
func TestV2UnenrollResumeHintNamesTheCommand(t *testing.T) {
	hint := v2UnenrollResumeHint("fe80::1%en35", unenrollAsset)
	for _, want := range []string{
		"wendy device unenroll --device fe80::1%en35 --asset-id " + unenrollAsset + " --yes",
		"wendy device unenroll --device fe80::1%en35 --force --yes",
	} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q lacks %q", hint, want)
		}
	}
	if hint := v2UnenrollResumeHint("", unenrollAsset); !strings.Contains(hint, "wendy device unenroll --asset-id "+unenrollAsset+" --yes") {
		t.Fatalf("default-device hint = %q", hint)
	}
	// The device argument is copied into a shell command: keep it one word.
	if hint := v2UnenrollResumeHint("evil; rm -rf ~", unenrollAsset); !strings.Contains(hint, "--device 'evil; rm -rf ~' --asset-id") {
		t.Fatalf("unquoted device in hint: %q", hint)
	}
}

// erasedUnfinishedReset is what performV2Unenroll returns when the device
// answers "credentials erased, cleanup incomplete": the reset happened.
func erasedUnfinishedReset() error {
	refusal := resetcleanup.Refusal(codes.Unavailable,
		"enrollment credentials erased; resource cleanup incomplete, restart will retry; reset completion unconfirmed",
		errors.New(`interface "wendy0" is not owned by wendy-net`))
	return fmt.Errorf("certificate revoked and asset deleted; local reset unconfirmed after 1 attempt(s): %w", refusal)
}

// stubClearSavedIdentity puts the command in a terminal (or not), answers the
// question, and records what was asked and which identities were cleared.
func stubClearSavedIdentity(t *testing.T, interactive, jsonMode, answer bool, forgetErr error) (asked, cleared *[]string) {
	t.Helper()
	prevTerminal, prevJSON, prevConfirm, prevForget := isInteractiveTerminalFn, jsonOutput, confirmFn, forgetResetDeviceIdentityFn
	t.Cleanup(func() {
		isInteractiveTerminalFn, jsonOutput, confirmFn, forgetResetDeviceIdentityFn = prevTerminal, prevJSON, prevConfirm, prevForget
	})
	asked, cleared = new([]string), new([]string)
	isInteractiveTerminalFn = func() bool { return interactive }
	jsonOutput = jsonMode
	confirmFn = func(question string) bool { *asked = append(*asked, question); return answer }
	forgetResetDeviceIdentityFn = func(keys ...string) error { *cleared = append(*cleared, keys...); return forgetErr }
	return asked, cleared
}

// A device that answers "credentials erased, cleanup incomplete" has been
// reset. The authenticated reset cannot be retried: the device no longer holds
// the identity that retry needs, and it serves only its setup port, which this
// computer refuses while it still has the device's identity saved. Clearing
// that saved identity is what a finished unenroll does, so in a terminal the
// CLI asks and does it, instead of naming a command for the user to type.
func TestV2UnenrollErasedButUnfinishedResetOffersToClearTheSavedIdentity(t *testing.T) {
	asked, cleared := stubClearSavedIdentity(t, true, false, true, nil)

	err := v2UnenrollErasedUnfinished(erasedUnfinishedReset(), "robot.local", "urn:wendy:device:robot")

	if len(*asked) != 1 || !strings.Contains((*asked)[0], "robot.local") {
		t.Fatalf("questions asked = %q, want one that names the device", *asked)
	}
	if !slices.Equal(*cleared, []string{"urn:wendy:device:robot"}) {
		t.Fatalf("identities cleared = %q, want the device's own", *cleared)
	}
	msg := err.Error()
	for _, want := range []string{"erased its enrollment", "saved identity for it is cleared", "wendy device enroll --device robot.local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("outcome %q lacks %q", msg, want)
		}
	}
	for _, unwanted := range []string{"wendy device unpin", "may still hold its enrollment keys", "--asset-id", "--force"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("outcome %q still names a step that is done or cannot run: %q", msg, unwanted)
		}
	}
	// The device's refusal still travels with the outcome, so the recovery
	// step for an unfinished cleanup is still offered.
	if len(ResetCleanupRecovery(err)) == 0 {
		t.Errorf("outcome lost the device's cleanup refusal: %v", err)
	}
}

// Declining leaves the saved identity alone and names the command that clears it.
func TestV2UnenrollErasedButUnfinishedResetNamesTheCommandWhenDeclined(t *testing.T) {
	asked, cleared := stubClearSavedIdentity(t, true, false, false, nil)

	msg := v2UnenrollErasedUnfinished(erasedUnfinishedReset(), "robot.local", "urn:wendy:device:robot").Error()

	if len(*asked) != 1 || len(*cleared) != 0 {
		t.Fatalf("asked %d time(s) and cleared %q, want one question and nothing cleared", len(*asked), *cleared)
	}
	for _, want := range []string{"erased its enrollment", "wendy device unpin robot.local", "wendy device enroll --device robot.local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("outcome %q lacks %q", msg, want)
		}
	}
}

// Without a terminal, and under --json, nothing is asked and nothing is
// cleared: the outcome names both commands.
func TestV2UnenrollErasedButUnfinishedResetNeverAsksWithoutATerminal(t *testing.T) {
	for name, mode := range map[string]struct{ interactive, json bool }{
		"no terminal": {false, false},
		"json":        {true, true},
	} {
		t.Run(name, func(t *testing.T) {
			asked, cleared := stubClearSavedIdentity(t, mode.interactive, mode.json, true, nil)

			msg := v2UnenrollErasedUnfinished(erasedUnfinishedReset(), "robot.local", "urn:wendy:device:robot").Error()

			if len(*asked) != 0 || len(*cleared) != 0 {
				t.Fatalf("asked %q and cleared %q, want neither", *asked, *cleared)
			}
			for _, want := range []string{"wendy device unpin robot.local", "wendy device enroll --device robot.local"} {
				if !strings.Contains(msg, want) {
					t.Errorf("outcome %q lacks %q", msg, want)
				}
			}
			for _, unwanted := range []string{"may still hold its enrollment keys", "--asset-id", "--force"} {
				if strings.Contains(msg, unwanted) {
					t.Errorf("outcome %q still offers a reset that cannot run: %q", msg, unwanted)
				}
			}
			// The device argument is copied into shell commands: keep it one word.
			if msg := v2UnenrollErasedUnfinished(erasedUnfinishedReset(), "evil; rm -rf ~").Error(); !strings.Contains(msg, "wendy device unpin 'evil; rm -rf ~'") {
				t.Errorf("unquoted device in outcome: %q", msg)
			}
		})
	}
}

// A clear that fails says so and still names the command, rather than
// reporting an identity as cleared that is not.
func TestV2UnenrollErasedButUnfinishedResetReportsAClearThatFailed(t *testing.T) {
	stubClearSavedIdentity(t, true, false, true, errors.New("config is read-only"))

	msg := v2UnenrollErasedUnfinished(erasedUnfinishedReset(), "robot.local", "urn:wendy:device:robot").Error()

	for _, want := range []string{"failed (config is read-only)", "wendy device unpin robot.local", "wendy device enroll --device robot.local"} {
		if !strings.Contains(msg, want) {
			t.Errorf("outcome %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "is cleared") {
		t.Errorf("outcome claims a clear that failed: %q", msg)
	}
}

// Only a device that says it erased its enrollment is offered the clear. Any
// other unconfirmed reset may still hold its keys, so it keeps the commands
// that finish the reset and its saved identity is left alone.
func TestV2UnenrollNotResetAsksOnlyAfterAnErasedReset(t *testing.T) {
	asked, cleared := stubClearSavedIdentity(t, true, false, true, nil)

	refused := fmt.Errorf("certificate revoked and asset deleted; local reset unconfirmed after 4 attempt(s): %w", errAgentRefusedReset)
	got := v2UnenrollNotReset(refused, "robot.local", unenrollAsset, "urn:wendy:device:robot").Error()
	if want := v2UnenrollUnfinished(refused, "robot.local", unenrollAsset).Error(); got != want {
		t.Errorf("outcome for a refused reset = %q, want the resume commands %q", got, want)
	}
	if len(*asked) != 0 || len(*cleared) != 0 {
		t.Fatalf("a refused reset asked %q and cleared %q, want neither", *asked, *cleared)
	}

	v2UnenrollNotReset(erasedUnfinishedReset(), "robot.local", unenrollAsset, "urn:wendy:device:robot")
	if len(*asked) != 1 || !slices.Equal(*cleared, []string{"urn:wendy:device:robot"}) {
		t.Fatalf("an erased reset asked %q and cleared %q, want one question and the device's identity cleared", *asked, *cleared)
	}
}

// A reset authorization refused only for clock skew is still Cloud-signed, so
// Cloud has deleted and revoked: say so and name the retry, not "uncertain".
func TestV2UnenrollClockSkewSaysCloudSideIsDone(t *testing.T) {
	var h resetRetryHarness
	skew := &unenrollproof.ClockSkewError{Offset: 14 * time.Second, Allowed: 10 * time.Second}
	ops := h.ops(t, func(int) (*v2AuthorizedReset, error) {
		return nil, fmt.Errorf("authenticating Cloud reset response: %w", skew)
	}, func(int) error { return nil }, nil)
	j := testUnenrollProgress()
	err := performV2Unenroll(context.Background(), &j, ops)
	if err == nil || !j.Deleted || j.Reset {
		t.Fatalf("err = %v, progress = %+v; want deleted, not reset", err, j)
	}
	msg := v2UnenrollUnfinished(err, "", j.AssetID).Error()
	for _, want := range []string{"about 14s behind Cloud's", "already removed this device and revoked its certificate", "Correct this computer's clock, then run: wendy device unenroll --asset-id " + unenrollAsset + " --yes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "failed/uncertain") {
		t.Errorf("message calls a finished Cloud side uncertain: %s", msg)
	}
}
