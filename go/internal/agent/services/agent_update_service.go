package services

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/sigverify"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type AgentUpdateService struct {
	agentpbv2.UnimplementedWendyAgentUpdateServiceServer
	logger    *zap.Logger
	installer *AgentInstaller
	online    *onlineUpdater

	// verifier checks the update binary's signature before install. Defaults
	// to sigverify.DefaultVerifier (disabled until a real pinned key is
	// embedded); settable within-package so tests can inject an enabled
	// verifier without changing the exported constructor signature.
	verifier *sigverify.Verifier

	execPathCache

	// restartFn is invoked once the binary is committed to schedule the
	// process exit that lets systemd restart into the new binary. Defaults
	// to scheduleAgentRestartExit (a real os.Exit(0) after a grace period);
	// settable within-package so tests exercising a successful install don't
	// kill the test process.
	restartFn func()
}

func NewAgentUpdateService(logger *zap.Logger, installer *AgentInstaller) *AgentUpdateService {
	return &AgentUpdateService{
		logger:    logger,
		installer: installer,
		online:    newOnlineUpdater(),
		verifier:  sigverify.DefaultVerifier,
		restartFn: scheduleAgentRestartExit,
	}
}

func (s *AgentUpdateService) UpdateAgent(stream grpc.BidiStreamingServer[agentpbv2.UpdateAgentRequest, agentpbv2.UpdateAgentResponse]) error {
	if !s.installer.TryLock() {
		return status.Error(codes.FailedPrecondition, "an update is already in progress")
	}
	// See AgentService.UpdateAgent for why committed is captured before the defer
	// and why the lock is not released on success.
	committed := false
	defer func() {
		if !committed {
			s.installer.Unlock()
		}
	}()

	s.logger.Info("UpdateAgent stream started")

	execPath, originalPerm, err := s.resolvedExecPath()
	if err != nil {
		return err
	}

	tmpFile, tmpPath, cleanupTmp, err := createUpdateTempFile(execPath)
	if err != nil {
		return err
	}
	defer func() {
		if !committed {
			cleanupTmp()
		}
	}()

	hasher := sha256.New()
	var written int64

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "error receiving update data: %v", err)
		}

		if chunk := msg.GetChunk(); chunk != nil {
			data := chunk.GetData()
			written += int64(len(data))
			if written > maxAgentBinarySize {
				return status.Errorf(codes.ResourceExhausted,
					"update stream exceeds maximum agent binary size (%d MiB)", maxAgentBinarySize>>20)
			}
			if _, err := tmpFile.Write(data); err != nil {
				return status.Errorf(codes.Internal, "failed to write update chunk: %v", err)
			}
			hasher.Write(data)
			continue
		}

		if ctrl := msg.GetControl(); ctrl != nil {
			if ctrl.GetUpdate() != nil {
				digest := hasher.Sum(nil)
				computedHash := hex.EncodeToString(digest)
				expectedHash := ctrl.GetUpdate().GetSha256()
				if expectedHash != "" && computedHash != expectedHash {
					return status.Errorf(codes.DataLoss,
						"SHA256 mismatch: expected %s, got %s", expectedHash, computedHash)
				}

				// Verify the binary's signature over its SHA256 digest before
				// installing. When the verifier is disabled (no pinned key
				// embedded yet), Verify is a fail-safe no-op and install
				// proceeds exactly as before this check existed.
				if err := s.verifier.Verify(digest, ctrl.GetUpdate().GetSignature()); err != nil {
					switch {
					case errors.Is(err, sigverify.ErrUnsigned):
						return status.Error(codes.FailedPrecondition, "agent update binary is unsigned; refusing install")
					case errors.Is(err, sigverify.ErrBadSignature):
						return status.Error(codes.DataLoss, "agent update binary signature verification failed; refusing install")
					default:
						return status.Errorf(codes.Internal, "agent update binary signature verification error: %v", err)
					}
				}

				if _, err := commitBinaryUpdate(tmpFile, tmpPath, execPath, computedHash, originalPerm, s.logger); err != nil {
					if errors.Is(err, ErrDirFsync) {
						s.logger.Warn("Update dir fsync failed; binary installed but rename may not survive power loss", zap.Error(err))
					} else {
						return err
					}
				}
				committed = true

				return finishCommittedUpdate(s.logger, func() error {
					return stream.Send(&agentpbv2.UpdateAgentResponse{
						ResponseType: &agentpbv2.UpdateAgentResponse_Updated_{
							Updated: &agentpbv2.UpdateAgentResponse_Updated{},
						},
					})
				}, s.restartFn)
			}
		}
	}

	return status.Error(codes.InvalidArgument, "update stream ended without update control command")
}
