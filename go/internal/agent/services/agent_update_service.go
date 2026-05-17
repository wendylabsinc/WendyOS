package services

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

type AgentUpdateService struct {
	agentpbv2.UnimplementedWendyAgentUpdateServiceServer
	logger     *zap.Logger
	updateMu   sync.Mutex
	isUpdating bool
}

func NewAgentUpdateService(logger *zap.Logger) *AgentUpdateService {
	return &AgentUpdateService{logger: logger}
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

	execPath, originalPerm, err := resolveExecPath()
	if err != nil {
		return err
	}

	tmpFile, tmpPath, cleanupTmp, err := createUpdateTempFile(execPath, originalPerm)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			cleanupTmp()
		}
	}()

	hasher := sha256.New()

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
			if _, err := tmpFile.Write(data); err != nil {
				return status.Errorf(codes.Internal, "failed to write update chunk: %v", err)
			}
			hasher.Write(data)
			continue
		}

		if ctrl := msg.GetControl(); ctrl != nil {
			if ctrl.GetUpdate() != nil {
				computedHash := hex.EncodeToString(hasher.Sum(nil))
				expectedHash := ctrl.GetUpdate().GetSha256()
				if expectedHash != "" && computedHash != expectedHash {
					return status.Errorf(codes.DataLoss,
						"SHA256 mismatch: expected %s, got %s", expectedHash, computedHash)
				}

				if _, err := commitBinaryUpdate(tmpFile, tmpPath, execPath, computedHash, s.logger); err != nil {
					return err
				}
				committed = true

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
