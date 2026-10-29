package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
