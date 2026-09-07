package vm

import (
	"net"
	"testing"
)

func TestNewVMsWithSameNameHaveDistinctStableMACs(t *testing.T) {
	one, two := newTestStore(t), newTestStore(t)
	createTestVM(t, one, "dev", Meta{})
	createTestVM(t, two, "dev", Meta{})
	first, err := one.MACAddress("dev")
	if err != nil {
		t.Fatal(err)
	}
	second, err := two.MACAddress("dev")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("same-name VMs share MAC %s", first)
	}
	for _, address := range []string{first, second} {
		mac, err := net.ParseMAC(address)
		if err != nil || len(mac) != 6 || mac[0]&3 != 2 {
			t.Fatalf("invalid local unicast MAC: %s", address)
		}
	}
	reopened := &Store{Root: one.Root}
	if got, err := reopened.MACAddress("dev"); err != nil || got != first {
		t.Fatalf("MAC changed: %s %v", got, err)
	}
	meta, _ := one.ReadMeta("dev")
	meta.AgentPort = 50100
	if err := one.WriteMeta(meta); err != nil {
		t.Fatal(err)
	}
	if got, err := one.MACAddress("dev"); err != nil || got != first {
		t.Fatal("port update changed MAC")
	}
}

func TestExistingVMKeepsLegacyMACAndRejectsCorruptIdentity(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	meta, _ := s.ReadMeta("dev")
	meta.MAC = ""
	if err := s.WriteMeta(meta); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MACAddress("dev"); err != nil || got != MACFor("dev") {
		t.Fatalf("legacy identity changed: %s %v", got, err)
	}
	for _, invalid := range []string{"invalid", "ff:ff:ff:ff:ff:ff", "00:11:22:33:44:55"} {
		meta.MAC = invalid
		if err := s.WriteMeta(meta); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MACAddress("dev"); err == nil {
			t.Fatalf("accepted corrupt identity %s", invalid)
		}
	}
}
