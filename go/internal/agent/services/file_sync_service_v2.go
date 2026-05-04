package services

import agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"

// FileSyncServiceV2 implements agentpbv2.WendyFileSyncServiceServer as a stub.
type FileSyncServiceV2 struct {
	agentpbv2.UnimplementedWendyFileSyncServiceServer
}

// NewFileSyncServiceV2 creates a new FileSyncServiceV2 stub.
func NewFileSyncServiceV2() *FileSyncServiceV2 {
	return &FileSyncServiceV2{}
}
