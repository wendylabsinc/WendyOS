package services

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePEMFiles(t *testing.T) {
	const (
		keyPEM   = "-----BEGIN EC PRIVATE KEY-----\nfake-key\n-----END EC PRIVATE KEY-----\n"
		certPEM  = "-----BEGIN CERTIFICATE-----\nfake-cert\n-----END CERTIFICATE-----\n"
		chainPEM = "-----BEGIN CERTIFICATE-----\nfake-chain\n-----END CERTIFICATE-----\n"
	)

	dir := t.TempDir()

	if err := WritePEMFiles(dir, keyPEM, certPEM, chainPEM); err != nil {
		t.Fatalf("WritePEMFiles: %v", err)
	}

	tests := []struct {
		filename string
		want     string
		wantPerm os.FileMode
	}{
		{"device-key.pem", keyPEM, 0o600},
		{"device.pem", certPEM, 0o644},
		{"ca.pem", chainPEM, 0o644},
	}

	for _, tc := range tests {
		path := filepath.Join(dir, tc.filename)

		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", tc.filename, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %q, want %q", tc.filename, string(got), tc.want)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", tc.filename, err)
			continue
		}
		gotPerm := info.Mode().Perm()
		if gotPerm != tc.wantPerm {
			t.Errorf("%s: got permissions %04o, want %04o", tc.filename, gotPerm, tc.wantPerm)
		}
	}
}

func TestWritePEMFiles_SkipsEmptyFields(t *testing.T) {
	dir := t.TempDir()

	// Only provide certPEM; key and chain are empty.
	if err := WritePEMFiles(dir, "", "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n", ""); err != nil {
		t.Fatalf("WritePEMFiles: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "device-key.pem")); !os.IsNotExist(err) {
		t.Error("device-key.pem should not exist when keyPEM is empty")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); !os.IsNotExist(err) {
		t.Error("ca.pem should not exist when chainPEM is empty")
	}
	if _, err := os.Stat(filepath.Join(dir, "device.pem")); err != nil {
		t.Errorf("device.pem should exist: %v", err)
	}
}
