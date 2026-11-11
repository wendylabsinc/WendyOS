package main

import (
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

func dialApplicationUDPWithCloudFallback(local, cloud func() (mesh.UDPFlow, error)) (mesh.UDPFlow, error) {
	flow, err := local()
	// The local dial rechecks the route before acquiring a session. An
	// ErrNoRoute leaves the caller's UDP endpoint and queued messages intact.
	if errors.Is(err, meshsession.ErrNoRoute) {
		return cloud()
	}
	return flow, err
}
