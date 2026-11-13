package commands

import (
	"testing"
)

func TestLocalMeshFlagsPreservePresenceAndIndependentCarriers(t *testing.T) {
	root := newNetCmd()
	cmd, _, err := root.Find([]string{"configure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := localMeshRequest(cmd); err == nil {
		t.Fatal("accepted empty partial update")
	}
	if err := cmd.ParseFlags([]string{"--participate=true", "--share-uplink=false", "--nan=false", "--ble=true", "--ethernet=false", "--infrastructure-wifi=true"}); err != nil {
		t.Fatal(err)
	}
	req, err := localMeshRequest(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if req.Participate == nil || !*req.Participate || req.ShareUplink == nil || *req.ShareUplink ||
		req.Nan == nil || *req.Nan || req.Ble == nil || !*req.Ble || req.Ethernet == nil || *req.Ethernet ||
		req.InfrastructureWifi == nil || !*req.InfrastructureWifi || req.Roam != nil {
		t.Fatalf("partial settings lost or carriers coupled: %+v", req)
	}
	if cmd.CommandPath() != "net configure" {
		t.Fatalf("command path = %q", cmd.CommandPath())
	}
}

func TestNetCommandRegistration(t *testing.T) {
	root := NewRootCmd()
	net, _, err := root.Find([]string{"net"})
	if err != nil || net.Name() != "net" || net.Hidden || net.GroupID != "manage" {
		t.Fatalf("net command is not visible in Manage: %v, %v", net, err)
	}
	for _, name := range []string{"configure", "status", "topology"} {
		cmd, remaining, err := root.Find([]string{"net", name})
		if err != nil || len(remaining) != 0 || cmd.CommandPath() != "wendy net "+name || cmd.RunE == nil {
			t.Fatalf("net %s failed to resolve: %v, %v, %v", name, cmd, remaining, err)
		}
		for _, flag := range []string{"device", "json"} {
			if cmd.InheritedFlags().Lookup(flag) == nil {
				t.Fatalf("net %s lost --%s", name, flag)
			}
		}
		for _, alias := range []string{"wendy-net", "local-mesh", "nan-mesh"} {
			legacy, remaining, err := root.Find([]string{"device", alias, name})
			if err != nil || len(remaining) != 0 || legacy.Name() != name || !legacy.Parent().Hidden {
				t.Fatalf("hidden compatibility path device %s %s failed: %v, %v", alias, name, legacy, err)
			}
		}
	}
}
