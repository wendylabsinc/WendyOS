package services

import (
	"context"
	"fmt"
)

// AcquireTwoPlaneNode keeps the two-plane data path running for owner, an
// agent-managed model host that carries no app labels and so never counts in
// the container sync, and returns the node carrying sourceID's frames once
// the first of them has reached it. The pin outlasts every container sync
// until ReleaseTwoPlaneNode; acquiring again for the same owner returns the
// source's current node.
//
// The pump decides on its first frame whether the source's stream can carry
// frame identity, so Acquire waits, within ctx, for that verdict. A source
// with no node (unknown, or refused before), a pump that stops instead of
// writing a frame (a refusal), or ctx ending first is an error, and leaves
// owner holding no pin.
func (s *VideoService) AcquireTwoPlaneNode(ctx context.Context, owner, sourceID string) (string, error) {
	s.twoPlaneMu.Lock()
	if s.twoPlanePinned == nil {
		s.twoPlanePinned = map[string]bool{}
	}
	s.twoPlanePinned[owner] = true
	s.twoPlaneDemand = true
	s.twoPlaneMu.Unlock()

	s.ensureTwoPlaneForLocalCameras(ctx)
	s.twoPlaneMu.Lock()
	node := s.twoPlane[sourceID]
	s.twoPlaneMu.Unlock()
	if node == nil {
		s.ReleaseTwoPlaneNode(ctx, owner)
		return "", fmt.Errorf("no two-plane node for %s: the camera is missing, or its stream cannot carry frame identity", sourceID)
	}
	if err := node.awaitFirstFrame(ctx, sourceID); err != nil {
		s.ReleaseTwoPlaneNode(ctx, owner)
		return "", err
	}
	return node.path, nil
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
		s.stopTwoPlaneIfUnneeded()
	}
}
