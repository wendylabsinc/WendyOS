package networkaccess

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, choice    string
		allows, wantErr bool
	}{
		{"existing edge installation", "", true, false},
		{"local", "local\n", false, false},
		{"network", "network\n", true, false},
		{"invalid fails closed", "yes", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.choice != "" {
				if err := os.WriteFile(filepath.Join(dir, "network-access"), []byte(tc.choice), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			p, err := Load(dir)
			if (err != nil) != tc.wantErr || p.AllowsNetwork != tc.allows {
				t.Fatalf("Load = %+v, %v", p, err)
			}
		})
	}
}

func TestLocalListeners(t *testing.T) {
	p := Policy{}
	registry, err := p.RegistryAddress("[::]:5555")
	if err != nil || registry != "127.0.0.1:5555" {
		t.Fatalf("registry = %q, %v", registry, err)
	}
	ephemeralRegistry, err := p.RegistryAddress("[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{p.Address("0"), ephemeralRegistry} {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		host, _, _ := net.SplitHostPort(listener.Addr().String())
		_ = listener.Close()
		if host != "127.0.0.1" {
			t.Fatalf("listener exposed on %s", host)
		}
	}
	if _, err := p.RegistryAddress("invalid"); err == nil {
		t.Fatal("invalid override accepted")
	}
	p.AllowsNetwork = true
	if p.Address("50051") != "[::]:50051" {
		t.Fatal("edge listener changed")
	}
	if got, err := p.RegistryAddress("10.1.2.3:5000"); err != nil || got != "10.1.2.3:5000" {
		t.Fatalf("explicit edge registry override lost: %q, %v", got, err)
	}
}

func TestAdvertisements(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"wendy-agent.service", "wendyos-mdns.service", "other.service"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := Policy{}
	changed, err := p.UpdateAdvertisements(dir)
	if err != nil || !changed {
		t.Fatalf("disable = %v, %v", changed, err)
	}
	for _, name := range []string{"wendy-agent.service", "wendyos-mdns.service"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still advertised: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "other.service")); err != nil {
		t.Fatal("unrelated service affected", err)
	}
	if changed, err := p.UpdateAdvertisements(dir); err != nil || changed {
		t.Fatalf("repeat disable = %v, %v", changed, err)
	}
	p.AllowsNetwork = true
	if changed, err := p.UpdateAdvertisements(dir); err != nil || !changed {
		t.Fatalf("restore = %v, %v", changed, err)
	}
	for _, name := range []string{"wendy-agent.service", "wendyos-mdns.service"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != name {
			t.Fatalf("metadata not restored: %s %v", data, err)
		}
	}
}
