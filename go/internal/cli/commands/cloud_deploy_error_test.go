package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCloudUploadFailureMessageAndClassification(t *testing.T) {
	for _, reconnect := range []error{nil, context.DeadlineExceeded, status.Error(codes.DeadlineExceeded, "private reconnect details")} {
		upload := status.Error(codes.Unavailable, "private chunk details")
		err := cloudChunkTransferError("jetson", chunkPushSnapshot{SentBytes: 1_500_000, PlannedBytes: 4_000_000}, upload, reconnect)
		for _, want := range []string{"The Cloud connection to jetson was lost during the upload.", "Sent 1.5 MB of 4.0 MB planned.", "The registry fallback isn't available over Cloud.", cloudUploadRetryHint} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q in %q", want, err)
			}
		}
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "attempt") || strings.Contains(err.Error(), "5000") || len(err.Error()) > 300 {
			t.Fatalf("verbose/internal message: %q", err)
		}
		if ErrorClass(err) != "device_unreachable" || ExecutionErrorClass(err) != "device_unreachable" {
			t.Fatalf("classes = %q/%q", ErrorClass(err), ExecutionErrorClass(err))
		}
		if !errors.Is(err, upload) || (reconnect != nil && !errors.Is(err, reconnect)) {
			t.Fatalf("lost causes: %v", err)
		}
		if steps := NextSteps(err); len(steps) != 1 || steps[0] != cloudUploadRetryHint {
			t.Fatalf("next steps=%v", steps)
		}
	}
}

func TestCloudPreparationFailuresDoNotClaimAnUploadDrop(t *testing.T) {
	for _, code := range []codes.Code{codes.Unimplemented, codes.Unavailable, codes.PermissionDenied} {
		cause := status.Error(code, "private preparation details")
		err := cloudChunkDeployFailure(&cloudImagePrepareError{cause})
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "registry") || strings.Contains(err.Error(), "lost") {
			t.Fatalf("preparation message/cause: %v", err)
		}
		if code == codes.Unimplemented && !strings.Contains(err.Error(), "Update the Agent, then retry.") {
			t.Fatalf("missing update hint: %v", err)
		}
		if retryableTunnelError(&cloudImagePrepareError{cause}) {
			t.Fatalf("preparation failure was classified as a tunnel drop: %v", cause)
		}
	}
	// Ordinary build/read failures must not acquire an irrelevant registry note.
	cause := io.ErrUnexpectedEOF
	if cloudChunkDeployFailure(cause) != cause {
		t.Fatal("rewrote an unrelated failure")
	}
	build := &imageBuildFailedError{status.Error(codes.Unimplemented, "builder is missing a capability")}
	if cloudChunkDeployFailure(build) != build {
		t.Fatal("rewrote a builder failure as an old Agent failure")
	}
}
