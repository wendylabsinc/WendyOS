package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/robotjoints"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
)

// TestJointStreamErrorsSeparateSilenceFromDeafness is the distinction the whole
// error mapping exists for: a robot that is not publishing and a device that
// could not listen send an operator to different machines, and a failure to
// decode is neither.
func TestJointStreamErrorsSeparateSilenceFromDeafness(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   codes.Code
		reason string
	}{
		{
			name:   "a silent robot",
			err:    fmt.Errorf("%w: nothing on /lowstate", robotjoints.ErrSourceAbsent),
			code:   codes.NotFound,
			reason: streamreason.RobotJointSourceAbsent,
		},
		{
			name: "a device that could not listen",
			err:  fmt.Errorf("%w: no wired interface", robotjoints.ErrNoInterface),
			code: codes.FailedPrecondition,
		},
		{
			name: "a backend this agent cannot read",
			err:  fmt.Errorf("%w %q", robotjoints.ErrUnknownBackend, "feetech-serial"),
			code: codes.InvalidArgument,
		},
		{
			name: "bytes that did not decode",
			err:  errors.New("decoding /lowstate: 8 bytes unconsumed"),
			code: codes.Internal,
		},
		{
			name: "the client going away, already a status",
			err:  status.Error(codes.Canceled, "context canceled"),
			code: codes.Canceled,
		},
		{
			name: "a stream the client simply stopped reading",
			err:  nil,
			code: codes.OK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jointStreamError("unitree-lowstate on /lowstate", tc.err)
			if status.Code(got) != tc.code {
				t.Fatalf("code = %v, want %v (err = %v)", status.Code(got), tc.code, got)
			}
			if tc.reason == "" {
				if info := streamreason.Info(got); info != nil {
					t.Errorf("carried reason %q, want none", info.GetReason())
				}
				return
			}
			if !streamreason.Has(got, tc.reason) {
				t.Fatalf("error carries no %s reason: a bare NOT_FOUND cannot be told from a "+
					"tunnel that lost the device", tc.reason)
			}
			if source := streamreason.Info(got).GetMetadata()["source"]; !strings.Contains(source, "/lowstate") {
				t.Errorf("reason metadata source = %q, want it to name what was listened for", source)
			}
		})
	}
}

// TestJointSampleLeavesIdleSlotsOut pins the wire contract: an idle slot is
// absent from the message rather than present as a zero angle, and the total
// stays beside the reporting ones so nothing is hidden by the rule.
func TestJointSampleLeavesIdleSlotsOut(t *testing.T) {
	at := time.Unix(0, 1234567890)
	sample := jointSampleProto(robotjoints.Reading{
		At:        at,
		SlotCount: 35,
		Unit:      "rad",
		Slots: []robotjoints.Slot{
			{Index: 0, Position: 0},
			{Index: 15, Position: 1.5},
		},
	})

	if sample.GetSlotCount() != 35 {
		t.Errorf("slot_count = %d, want 35", sample.GetSlotCount())
	}
	if got := len(sample.GetSlots()); got != 2 {
		t.Fatalf("slots = %d, want 2 — the idle ones must not be filled in", got)
	}
	if sample.GetSlots()[0].GetIndex() != 0 || sample.GetSlots()[0].GetPosition() != 0 {
		t.Error("a driven joint sitting at exactly zero must still be reported")
	}
	if sample.GetSlots()[1].GetIndex() != 15 {
		t.Error("indices must not be renumbered to close the gaps")
	}
	if sample.GetObservedAtUnixNanos() != at.UnixNano() || sample.GetUnit() != "rad" {
		t.Errorf("sample = %+v, want the observation time and unit carried", sample)
	}
}

// TestAgentWithoutAPoolRefusesTheJointStream: an agent that cannot join a DDS
// domain says so, rather than answering as though the robot were quiet.
func TestAgentWithoutAPoolRefusesTheJointStream(t *testing.T) {
	svc := NewRobotService(zap.NewNop(), t.TempDir(), nil)
	err := svc.StreamJointPositions(nil, nil)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err = %v)", status.Code(err), err)
	}
	if streamreason.Has(err, streamreason.RobotJointSourceAbsent) {
		t.Error("an agent that cannot listen must not report a silent robot")
	}
}
