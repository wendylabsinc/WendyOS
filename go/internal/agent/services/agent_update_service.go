package services

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/agent/gpgverify"
	"github.com/wendylabsinc/wendy/internal/shared/releasekeys"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

type AgentUpdateService struct {
	agentpbv2.UnimplementedWendyAgentUpdateServiceServer
	logger       *zap.Logger
	updateMu     sync.Mutex
	isUpdating   bool
	gpgPublicKey []byte
}

func NewAgentUpdateService(logger *zap.Logger) *AgentUpdateService {
	return &AgentUpdateService{
		logger:       logger,
		gpgPublicKey: releasekeys.WendyReleasesPublicKey,
	}
}

func (s *AgentUpdateService) UpdateAgent(stream grpc.BidiStreamingServer[agentpbv2.UpdateAgentRequest, agentpbv2.UpdateAgentResponse]) error {
	s.updateMu.Lock()
	if s.isUpdating {
		s.updateMu.Unlock()
		return status.Error(codes.FailedPrecondition, "an update is already in progress")
	}
	s.isUpdating = true
	s.updateMu.Unlock()

	defer func() {
		s.updateMu.Lock()
		s.isUpdating = false
		s.updateMu.Unlock()
	}()

	s.logger.Info("UpdateAgent stream started")

	hasher := sha256.New()
	var binaryData []byte

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "error receiving update data: %v", err)
		}

		if chunk := msg.GetChunk(); chunk != nil {
			binaryData = append(binaryData, chunk.GetData()...)
			hasher.Write(chunk.GetData())
			continue
		}

		if ctrl := msg.GetControl(); ctrl != nil {
			if ctrl.GetUpdate() != nil {
				updateCmd := ctrl.GetUpdate()
				computedHash := hex.EncodeToString(hasher.Sum(nil))
				expectedHash := updateCmd.GetSha256()
				if expectedHash != "" && computedHash != expectedHash {
					return status.Errorf(codes.DataLoss,
						"SHA256 mismatch: expected %s, got %s", expectedHash, computedHash)
				}

				// Skipping is only possible when the agent itself was compiled
				// with the wendy_dev_skip_gpg build tag — it is never
				// controllable by the update request.
				if gpgverify.SkipVerificationAllowed {
					s.logger.Warn("GPG verification skipped (developer build: wendy_dev_skip_gpg)")
				} else {
					if len(updateCmd.GetGpgSignature()) == 0 {
						return status.Error(codes.PermissionDenied, "update rejected: GPG signature is required")
					}
					if err := gpgverify.VerifyBinary(binaryData, updateCmd.GetGpgSignature(), s.gpgPublicKey); err != nil {
						return status.Errorf(codes.PermissionDenied, "update rejected: %v", err)
					}
				}

				execPath, err := os.Executable()
				if err != nil {
					return status.Errorf(codes.Internal, "failed to get executable path: %v", err)
				}
				execPath, err = filepath.EvalSymlinks(execPath)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to resolve executable symlinks: %v", err)
				}

				info, err := os.Stat(execPath)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to stat executable: %v", err)
				}
				originalPerm := info.Mode()

				tmpPath := execPath + ".update"
				if err := os.WriteFile(tmpPath, binaryData, originalPerm); err != nil {
					return status.Errorf(codes.Internal, "failed to write update file: %v", err)
				}

				backupPath := execPath + ".backup"
				if err := os.Rename(execPath, backupPath); err != nil {
					os.Remove(tmpPath)
					return status.Errorf(codes.Internal, "failed to create backup: %v", err)
				}

				if err := os.Rename(tmpPath, execPath); err != nil {
					if rbErr := os.Rename(backupPath, execPath); rbErr != nil {
						s.logger.Error("Failed to rollback from backup",
							zap.Error(rbErr),
							zap.String("backup_path", backupPath),
						)
					}
					os.Remove(tmpPath)
					return status.Errorf(codes.Internal, "failed to replace binary: %v", err)
				}

				s.logger.Info("Agent binary updated successfully",
					zap.String("sha256", computedHash),
					zap.Int("size", len(binaryData)),
				)

				if err := stream.Send(&agentpbv2.UpdateAgentResponse{
					ResponseType: &agentpbv2.UpdateAgentResponse_Updated_{
						Updated: &agentpbv2.UpdateAgentResponse_Updated{},
					},
				}); err != nil {
					return err
				}

				go func() {
					time.Sleep(500 * time.Millisecond)
					os.Exit(0)
				}()

				return nil
			}
		}
	}

	return status.Error(codes.InvalidArgument, "update stream ended without update control command")
}
