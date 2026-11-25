package agentmodule

import (
	"context"
	"testing"
)

func TestReplicaModuleOneWayReader(t *testing.T) {
	a, b := newReplicaPairConfigured(t, func(index int, c *Config) {
		if index == 0 {
			c.Replication.Peers[0].Import = nil
			c.Policy.Grants = c.Policy.Grants[:1]
		} else {
			c.Replication.Peers[0].Export = nil
			reader := c.Policy.Grants[1]
			reader.Subscribe = reader.Subscribe[1:]
			c.Policy.Grants = []Grant{reader}
		}
	})
	startReplicaPair(t, a, b)
	writer := a.publisher(t, 0, 1)
	a.publish(t, writer, replicaRequest())
	b.sync(t)
	values, err := b.m.replica.peers[a.cert.Principal].store.Snapshot(context.Background())
	if err != nil || len(values) != 1 {
		t.Fatal(values, err)
	}
	result, err := a.m.SyncPeer(context.Background(), b.cert.Principal)
	if err != nil || !result.Complete || result.Pages != 0 {
		t.Fatal(result, err)
	}
	c := a.config.Replication.Peers[0]
	c.Export = nil
	config := ReplicaConfig{AuthorityPath: a.config.Replication.AuthorityPath, Peers: []ReplicaPeer{c}}
	if config.validate(a.config.Policy) == nil {
		t.Fatal("peer with no directions accepted")
	}
}
