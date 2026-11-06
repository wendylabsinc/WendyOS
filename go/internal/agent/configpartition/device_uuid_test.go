package configpartition

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func certWithURI(t *testing.T, uri string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{u}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestPersistDeviceUUIDWritesTheCertUUIDAndMDNSID(t *testing.T) {
	const device = "019535d9-3df7-79fb-b466-fa907fa17f9e"
	dir := t.TempDir()
	uuidPath := filepath.Join(dir, "etc", "wendyos", "device-uuid")
	serviceDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(serviceDir, "wendy-agent.service")
	if err := os.WriteFile(service, []byte("<service><type>_wendyos._udp</type><txt-record>id=first-boot-uuid</txt-record><txt-record>name=box</txt-record></service>"), 0o644); err != nil {
		t.Fatal(err)
	}

	cert := certWithURI(t, "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/"+device)
	if err := persistDeviceUUID(dir, serviceDir, cert); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(uuidPath); strings.TrimSpace(string(got)) != device {
		t.Fatalf("device-uuid = %q, want %s", got, device)
	}
	got, _ := os.ReadFile(service)
	if !strings.Contains(string(got), "<txt-record>id="+device+"</txt-record>") || !strings.Contains(string(got), "name=box") {
		t.Fatalf("mDNS id= not the cert UUID: %s", got)
	}

	// An unnamed device kept the hostname it answered by before the UUID changed.
	hostname, _ := os.Hostname()
	if got, _ := os.ReadFile(filepath.Join(dir, "etc", "wendy-agent", "hostname")); strings.TrimSpace(string(got)) != hostname {
		t.Fatalf("hostname not pinned before the UUID changed: %q", got)
	}

	// A legacy asset certificate has no device UUID: nothing is rewritten.
	legacyRoot := t.TempDir()
	if err := persistDeviceUUID(legacyRoot, serviceDir, certWithURI(t, "urn:wendy:org:7:asset:42")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "etc")); !os.IsNotExist(err) {
		t.Fatalf("legacy certificate wrote identity files: %v", err)
	}
}

func TestPersistDeviceUUIDPinsHostnameUnlessNamed(t *testing.T) {
	hostname, _ := os.Hostname()
	cases := []struct {
		name, file, content string
		wantPin             string // "" = explicit hostname file must stay as written
	}{
		{"device-name set", "etc/wendyos/device-name", "box-01\n", ""},
		{"explicit hostname set", "etc/wendy-agent/hostname", "box-01\n", ""},
		// generate-hostname.sh ignores an empty name file and would rename the device.
		{"empty device-name", "etc/wendyos/device-name", "", hostname},
		{"whitespace explicit hostname", "etc/wendy-agent/hostname", " \n", hostname},
	}
	cert := certWithURI(t, "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/019535d9-3df7-79fb-b466-fa907fa17f9e")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			named := filepath.Join(dir, tc.file)
			if err := os.MkdirAll(filepath.Dir(named), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(named, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := persistDeviceUUID(dir, t.TempDir(), cert); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "etc", "wendy-agent", "hostname"))
			switch {
			case tc.wantPin != "" && strings.TrimSpace(string(got)) != tc.wantPin:
				t.Fatalf("hostname not pinned: %q", got)
			case tc.wantPin == "" && tc.file != "etc/wendy-agent/hostname" && !os.IsNotExist(err):
				t.Fatalf("named device got an explicit hostname: %q, %v", got, err)
			case tc.wantPin == "" && tc.file == "etc/wendy-agent/hostname" && string(got) != tc.content:
				t.Fatalf("explicit hostname rewritten: %q", got)
			}
		})
	}
}

func TestPersistDeviceUUIDRefusesHostnameDirectoryBeforeChangingIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "etc/wendyos/device-uuid")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old-boot-identity\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/wendy-agent/hostname"), 0700); err != nil {
		t.Fatal(err)
	}
	cert := certWithURI(t, "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/019535d9-3df7-79fb-b466-fa907fa17f9e")
	if err := persistDeviceUUID(root, t.TempDir(), cert); err == nil {
		t.Fatal("changed identity without a usable hostname pin")
	}
	if got, _ := os.ReadFile(path); string(got) != "old-boot-identity\n" {
		t.Fatalf("failed migration changed device UUID: %q", got)
	}
}

func TestPersistDeviceUUIDPinsHostnameWhenNameFilesAreEmpty(t *testing.T) {
	for _, name := range []string{"etc/wendy-agent/hostname", "etc/wendyos/device-name"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("\n"), 0644); err != nil {
				t.Fatal(err)
			}
			cert := certWithURI(t, "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/019535d9-3df7-79fb-b466-fa907fa17f9e")
			if err := persistDeviceUUID(root, t.TempDir(), cert); err != nil {
				t.Fatal(err)
			}
			hostname, _ := os.Hostname()
			if got, _ := os.ReadFile(filepath.Join(root, "etc/wendy-agent/hostname")); strings.TrimSpace(string(got)) != hostname {
				t.Fatalf("UUID change lost automatic hostname: %q", got)
			}
		})
	}
}

func TestPersistDeviceUUIDKeepsPersistenceSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "persistent-uuid")
	if err := os.WriteFile(target, []byte("old-id\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "etc/wendyos/device-uuid")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	const device = "019535d9-3df7-79fb-b466-fa907fa17f9e"
	cert := certWithURI(t, "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/"+device)
	if err := persistDeviceUUID(root, t.TempDir(), cert); err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink(path); err != nil || link != target {
		t.Fatalf("persistence link replaced: %q, %v", link, err)
	}
	if got, _ := os.ReadFile(target); string(got) != device+"\n" {
		t.Fatalf("persistent UUID not updated: %q", got)
	}
}
