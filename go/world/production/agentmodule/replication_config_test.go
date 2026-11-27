package agentmodule

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestReplicaModuleCommissioningExactScopesAndProtectedStores(t *testing.T) {
	a, _ := newReplicaPair(t)
	for name, change := range map[string]func(*Config){
		"no peer generation":     func(c *Config) { c.Replication.Peers[0].Generation = 0 },
		"local as remote":        func(c *Config) { c.Replication.Peers[0].Device = c.Policy.Device },
		"fingerprint spelling":   func(c *Config) { c.Replication.Peers[0].CredentialSHA256 = "not-a-fingerprint" },
		"relative store":         func(c *Config) { c.Replication.AuthorityPath = "authority.db" },
		"shared local store":     func(c *Config) { c.Replication.Peers[0].RemotePath = c.JournalPath },
		"shared authority store": func(c *Config) { c.Replication.Peers[0].RemotePath = c.Replication.AuthorityPath },
		"foreign export":         func(c *Config) { c.Replication.Peers[0].Export[0].Device = c.Replication.Peers[0].Device },
		"third origin import":    func(c *Config) { c.Replication.Peers[0].Import[0].Device = c.Policy.Device },
		"media import":           func(c *Config) { c.Replication.Peers[0].Import[0].Component = "wendy.sensor.audio.v1" },
		"duplicate import": func(c *Config) {
			c.Replication.Peers[0].Import = append(c.Replication.Peers[0].Import, c.Replication.Peers[0].Import[0])
		},
		"partial original delegation": func(c *Config) {
			c.Policy.Grants[0].Publish = append(c.Policy.Grants[0].Publish, serviceorigin.PublishScope{ComponentType: "wendy.sensor.scalar.v1", Stream: "temperature"})
		},
		"uncommissioned subscription": func(c *Config) { c.Policy.Grants[1].Subscribe[1].Stream = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(a.config)
			var cfg Config
			_ = json.Unmarshal(raw, &cfg)
			change(&cfg)
			if cfg.Policy.validateWithReplication(cfg.Replication) == nil && cfg.Replication.validate(cfg.Policy, cfg.JournalPath, cfg.ProofArchivePath) == nil {
				t.Fatal("invalid replication commission accepted")
			}
		})
	}
	devices := a.m.ReplicaDevices()
	devices[0] = "mutated"
	if a.m.ReplicaDevices()[0] == "mutated" {
		t.Fatal("peer accessor leaked config ownership")
	}
}

func TestReplicaModuleNoConfigurationIsDisabled(t *testing.T) {
	var r *ReplicaConfig
	if r.validate(basePolicy()) != nil || r.imports(basePolicy(), accessSource("sensor", "wendy.sensor.scalar.v1", "gas")) {
		t.Fatal("nil commission acquired remote scope")
	}
	m := &Manager{}
	if m.ReplicaService() != nil || len(m.ReplicaDevices()) != 0 || m.StartReplication(context.Background(), ReplicaTransport{}) != nil || m.RefreshReplication(context.Background()) != nil {
		t.Fatal("disabled module changed native lifecycle")
	}
}

func TestReplicaModulePeerPolicyGenerationAndDeniedCertificate(t *testing.T) {
	a, b := newReplicaPair(t)
	startReplicaPair(t, a, b)
	id, e := a.m.AuthenticateReplicaPeer(context.Background(), b.cert.ChainDER)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.m.replicaCurrent(context.Background(), id, 1, 2); e == nil {
		t.Fatal("wrong policy generation accepted")
	}
	w, _ := a.clock.Now()
	source := a.config.Replication.Peers[0].Import[0]
	source.Stream = "not-granted"
	if _, e := a.m.replicaPeerWindow(context.Background(), id, 1, 1, []proofref.Source{source}, w); e == nil {
		t.Fatal("peer enrollment implied source permission")
	}
	a.m.cfg.Policy.DeniedCertificates = []string{replicaFingerprint(b.cert.ChainDER)}
	if e = a.m.replicaCurrent(context.Background(), id, 1, 1); e == nil {
		t.Fatal("cached TLS chain bypassed current deny policy")
	}
}
