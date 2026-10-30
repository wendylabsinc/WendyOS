//go:build js && wasm

package main

import (
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
)

func TestLocalCLIReporterDoesNotAttemptPrivateIPCInBrowser(t *testing.T) {
	// No auth Session, browser location or relay is installed. The private native
	// source must produce an explicit unavailable observation before those paths.
	result, err := (&bridge{}).handle(request{Method: "mesh-topology", Params: json.RawMessage(`{"id":"cli://local"}`)})
	if err != nil {
		t.Fatal(err)
	}
	observation, ok := result.(browserauth.TopologyObservation)
	if !ok || observation.Status != "unavailable" || observation.Platform != "wendy-cli" || observation.Topology != nil {
		t.Fatal(result)
	}
}
