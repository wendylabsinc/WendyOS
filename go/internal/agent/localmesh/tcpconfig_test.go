package localmesh

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTCPConfigRequiresReciprocalConcretePeers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-mesh.json")
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"listen":"127.0.0.1:7001","peers":[{"asset":2,"address":"127.0.0.1:7002"}]}`, true},
		{`{"listen":"127.0.0.1:7001","peers":[{"asset":1,"address":"127.0.0.1:7002"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[{"asset":2,"address":"0.0.0.0:7002"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[{"asset":2,"address":"127.0.0.1:7002"},{"asset":2,"address":"127.0.0.1:7003"}]}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[],"unknown":true}`, false},
		{`{"listen":"127.0.0.1:7001","peers":[]} {}`, false},
		{`{"nan":true}`, true},
		{`{"nan":true,"peers":[{"asset":2,"address":"127.0.0.1:7002"}]}`, false},
		{`{"peers":[]}`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadTCPConfig(path, 1)
		if (err == nil && cfg != nil) != tc.valid {
			t.Errorf("body %s: valid=%v, err=%v", tc.body, tc.valid, err)
		}
	}
}
