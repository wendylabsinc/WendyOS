package containerd

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"testing"
)

func TestLANReplyGuardInventory(t *testing.T) {
	key := appconfig.EntitlementAnnotationKeyPrefix + "network"
	for _, tc := range []struct {
		name, value string
		mesh, bad   bool
	}{
		{"legacy mesh", "mode=mesh,servicecidr=10.99.0.0/16", true, false},
		{"JSON mesh", `{"mode":"mesh","serviceCIDR":"10.99.0.0/16"}`, true, false},
		{"ordinary host", "mode=host", false, false}, {"ordinary bridge", "mode=bridge", false, false}, {"implicit host", "", false, false},
		{"bad JSON", `{"mode":"mesh"`, false, true}, {"case duplicate mode", `{"Mode":"mesh","mode":"host"}`, false, true},
		{"duplicate mode", `{"mode":"mesh","mode":"host"}`, false, true},
		{"unknown mode", `{"mode":"mes"}`, false, true}, {"unknown field", `{"mdoe":"mesh"}`, false, true},
		{"bad legacy", "mode=mesh,broken", false, true}, {"duplicate legacy", "mode=mesh,mode=host", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := lanGuardNetworks(map[string]map[string]string{"org.example.app": {key: tc.value}}, map[string]string{"org.example.app": "10.42.2.0/28"})
			if (e != nil) != tc.bad {
				t.Fatalf("error=%v", e)
			}
			if !tc.bad && (len(got) == 1) != tc.mesh {
				t.Fatalf("networks=%v", got)
			}
		})
	}
	// Inventory is container-based: no task/PID exists in this proof. A retained
	// mesh container owns its subnet even while stopped or between task attempts.
	inv := map[string]map[string]string{"org.example.app": {key: "mode=mesh"}}
	if _, e := lanGuardNetworks(inv, map[string]string{}); e == nil {
		t.Fatal("missing allocation permitted guard removal")
	}
	if got, e := lanGuardNetworks(map[string]map[string]string{}, map[string]string{"org.example.app": "10.42.2.0/28"}); e != nil || len(got) != 0 {
		t.Fatalf("deleted owner: %v %v", got, e)
	}
	if got, e := lanGuardNetworks(map[string]map[string]string{"ordinary": {}}, map[string]string{}); e != nil || len(got) != 0 {
		t.Fatalf("ordinary container: %v %v", got, e)
	}
}
