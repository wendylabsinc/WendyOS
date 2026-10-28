package operatormesh

import (
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func TestOperatorEndpointNormalPrivateStateUpgradeKeepsInstanceAndRevision(t *testing.T) {
	operator, err := meshoperator.ParsePrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	if err != nil {
		t.Fatal(err)
	}
	instance := meshoperator.InstanceID{1, 2, 3, 4, 5}
	original := struct {
		Version   int                     `json:"version"`
		Principal string                  `json:"principal"`
		Instance  meshoperator.InstanceID `json:"instance"`
		Revision  uint64                  `json:"revision"`
	}{Version: 1, Principal: operator.Principal(), Instance: instance, Revision: 997}
	wire, err := netwire.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(endpointStatePath(directory, operator), wire, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded, release, err := reserveEndpoint(directory, operator)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if upgraded.Instance != instance || upgraded.Principal != original.Principal || upgraded.Revision != 998 {
		t.Fatal("private state reset during upgrade", upgraded)
	}
	stored, err := os.ReadFile(endpointStatePath(directory, operator))
	if err != nil {
		t.Fatal(err)
	}
	var compact endpointState
	if err := netwire.UnmarshalSigned(stored, &compact); err != nil {
		t.Fatal("upgraded private state not compact", err)
	}
	if compact != upgraded {
		t.Fatal("upgraded private state differs", compact)
	}
	restarted, release, err := reserveEndpoint(directory, operator)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if restarted.Instance != instance || restarted.Revision != 999 {
		t.Fatal("restart did not continue upgraded revision", restarted)
	}
}
