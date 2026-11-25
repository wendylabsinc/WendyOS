package agentmodule

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

func TestReplicaSyncIntervalUsesCommissionedComponents(t *testing.T) {
	if (*Manager)(nil).ReplicaSyncInterval() != 2*time.Second || (&Manager{}).ReplicaSyncInterval() != 2*time.Second {
		t.Fatal("disabled replication changed its state-only default")
	}
	for _, direction := range []string{"import", "export"} {
		for _, component := range []string{replication.ScalarComponent, sensorrecord.ComponentType, resultrecord.ComponentType} {
			t.Run(direction+"/"+component, func(t *testing.T) {
				peer := ReplicaPeer{}
				sources := []proofref.Source{{Component: component}}
				if direction == "import" {
					peer.Import = sources
				} else {
					peer.Export = sources
				}
				// A prior state-only peer must not hide a later live-data peer.
				m := &Manager{cfg: Config{Replication: &ReplicaConfig{Peers: []ReplicaPeer{
					{Import: []proofref.Source{{Component: replication.ScalarComponent}}}, peer,
				}}}}
				want := 250 * time.Millisecond
				if component == replication.ScalarComponent {
					want = 2 * time.Second
				}
				if got := m.ReplicaSyncInterval(); got != want {
					t.Fatalf("interval = %s, want %s", got, want)
				}
			})
		}
	}
}
