package services

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
)

func TestParseTegraVersions(t *testing.T) {
	v, ok := parseTegraVersions("# R36 (release), REVISION: 4.3", "Current version: 36.4.3")
	if !ok || v.RootfsFamily != "36" || v.BootFamily != "36" || v.RootfsVersion != "R36.4.3" || v.BootVersion != "R36.4.3" {
		t.Fatalf("unexpected parse: %+v, ok=%v", v, ok)
	}
}

func TestTegraCSIPreflightMismatchHasErrorInfo(t *testing.T) {
	s := NewVideoService(context.Background(), zap.NewNop(), nil)
	s.readTegraRelease = func() ([]byte, error) { return []byte("# R38 (release), REVISION: 2.0"), nil }
	s.dumpBootSlots = func(context.Context) ([]byte, error) { return []byte("Current version: 36.4.3"), nil }
	err := s.preflightTegraCSI(context.Background())
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", st.Code())
	}
	info := streamreason.Info(err)
	if info == nil {
		t.Fatal("missing ErrorInfo")
	}
	if info.GetReason() != tegraFirmwareMismatchReason ||
		info.GetMetadata()["rootfs_l4t"] != "R38.2.0" ||
		info.GetMetadata()["boot_firmware_l4t"] != "R36.4.3" {
		t.Fatalf("unexpected ErrorInfo: %+v", info)
	}
}

func TestTegraCSIPreflightUnknownDoesNotBlock(t *testing.T) {
	s := NewVideoService(context.Background(), zap.NewNop(), nil)
	s.readTegraRelease = func() ([]byte, error) { return nil, errors.New("missing") }
	s.dumpBootSlots = func(context.Context) ([]byte, error) { return nil, errors.New("missing") }
	if err := s.preflightTegraCSI(context.Background()); err != nil {
		t.Fatalf("unknown state blocked CSI: %v", err)
	}
}
