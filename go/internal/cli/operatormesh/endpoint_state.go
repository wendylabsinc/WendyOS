package operatormesh

import (
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// Endpoint installation state is private durable data, not a mesh frame. Keep
// the instance and monotonically increasing counter when upgrading the local
// CBOR schema; losing either can change the operator address or reuse a proof
// revision. Network decoders continue to accept only the current wire format.
func decodeEndpointState(data []byte) (endpointState, error) {
	var state endpointState
	currentError := netwire.UnmarshalSigned(data, &state)
	if currentError == nil {
		return state, nil
	}
	// The original private file used the Go/JSON field spelling before explicit
	// compact CBOR tags were added. Its version and principal are checked by the
	// owner before the migrated state is incremented and atomically persisted.
	var original struct {
		Version   int                     `json:"version"`
		Principal string                  `json:"principal"`
		Instance  meshoperator.InstanceID `json:"instance"`
		Revision  uint64                  `json:"revision"`
	}
	if err := netwire.UnmarshalSigned(data, &original); err != nil {
		return endpointState{}, fmt.Errorf("decode local operator endpoint state: %w", errors.Join(currentError, err))
	}
	return endpointState{Version: original.Version, Principal: original.Principal, Instance: original.Instance, Revision: original.Revision}, nil
}
