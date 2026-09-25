package services

import (
	"context"
	"fmt"
)

// AcquireTwoPlaneNode keeps the two-plane data path running for owner, an
// agent-managed model host that carries no app labels and so never counts in
// the container sync, and returns the node carrying sourceID's frames. The
// pin outlasts every container sync until ReleaseTwoPlaneNode. A source with
// no node (unknown, or refused because its stream cannot carry frame
// identity) is an error and leaves no pin behind.
func (s *VideoService) AcquireTwoPlaneNode(ctx context.Context, owner, sourceID string) (string, error) {
	s.twoPlaneMu.Lock()
	if s.twoPlanePinned == nil {
		s.twoPlanePinned = map[string]bool{}
	}
	s.twoPlanePinned[owner] = true
	s.twoPlaneDemand = true
	s.twoPlaneMu.Unlock()

	s.ensureTwoPlaneForLocalCameras(ctx)
	if node, ok := s.TwoPlaneNodePath(sourceID); ok {
		return node, nil
	}
	s.ReleaseTwoPlaneNode(ctx, owner)
	return "", fmt.Errorf("no two-plane node for %s: the camera is missing, or its stream cannot carry frame identity", sourceID)
}

// ReleaseTwoPlaneNode drops owner's pin, and stops the data path when nothing
// else needs it.
func (s *VideoService) ReleaseTwoPlaneNode(_ context.Context, owner string) {
	s.twoPlaneMu.Lock()
	delete(s.twoPlanePinned, owner)
	s.twoPlaneDemand = s.twoPlaneContainerDemand || len(s.twoPlanePinned) > 0
	demand := s.twoPlaneDemand
	s.twoPlaneMu.Unlock()
	if !demand {
		s.stopAllTwoPlane()
	}
}
