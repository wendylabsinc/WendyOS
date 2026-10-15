package commands

import "testing"

func TestLocalMeshFlagsPreservePresenceAndIndependentCarriers(t *testing.T) {
	root := newDeviceLocalMeshCmd()
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
	if len(root.Aliases) != 1 || root.Aliases[0] != "nan-mesh" {
		t.Fatal("legacy nan-mesh alias missing")
	}
}
