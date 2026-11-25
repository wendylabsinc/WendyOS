package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func x86VarsSpec(t *testing.T, template []byte) (Spec, string) {
	t.Helper()
	dir := t.TempDir()
	tmpl := filepath.Join(t.TempDir(), "OVMF_VARS.fd")
	if template != nil {
		if err := os.WriteFile(tmpl, template, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Spec{Name: "dev", Arch: ArchAMD64, FirmwareVars: filepath.Join(dir, "efivars.fd"), FirmwareVarsTemplate: tmpl}, tmpl
}

func TestEnsureVarsStore(t *testing.T) {
	t.Run("arm64 is left alone", func(t *testing.T) {
		store := filepath.Join(t.TempDir(), "efivars.fd")
		if err := ensureVarsStore(Spec{FirmwareVars: store}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(store); !os.IsNotExist(err) {
			t.Fatalf("ARM64 store was written: %v", err)
		}
		if err := os.WriteFile(store, []byte("arm64 vars"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureVarsStore(Spec{Arch: ArchARM64, FirmwareVars: store, FirmwareVarsTemplate: store + ".tmpl"}); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(store); string(got) != "arm64 vars" {
			t.Fatalf("ARM64 store changed to %q", got)
		}
	})
	t.Run("missing store is copied from the template", func(t *testing.T) {
		template := bytes.Repeat([]byte{0xA5}, 4096)
		spec, _ := x86VarsSpec(t, template)
		if err := ensureVarsStore(spec); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(spec.FirmwareVars)
		if err != nil || !bytes.Equal(got, template) {
			t.Fatalf("store = %d bytes, %v; want a copy of the template", len(got), err)
		}
		if info, _ := os.Stat(spec.FirmwareVars); info.Mode().Perm() != 0o600 {
			t.Errorf("store mode = %v, want 0600", info.Mode().Perm())
		}
		entries, _ := os.ReadDir(filepath.Dir(spec.FirmwareVars))
		if len(entries) != 1 {
			t.Errorf("VM directory holds %d entries after the copy, want only the store", len(entries))
		}
	})
	t.Run("store at the template's size is kept", func(t *testing.T) {
		spec, _ := x86VarsSpec(t, bytes.Repeat([]byte{0xA5}, 4096))
		kept := bytes.Repeat([]byte{0x5A}, 4096)
		if err := os.WriteFile(spec.FirmwareVars, kept, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureVarsStore(spec); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(spec.FirmwareVars); !bytes.Equal(got, kept) {
			t.Fatal("a store already sized for its template was replaced")
		}
	})
	t.Run("store at another size is replaced", func(t *testing.T) {
		template := bytes.Repeat([]byte{0xA5}, 4096)
		spec, _ := x86VarsSpec(t, template)
		if err := os.WriteFile(spec.FirmwareVars, bytes.Repeat([]byte{0x5A}, 2048), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureVarsStore(spec); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(spec.FirmwareVars); !bytes.Equal(got, template) {
			t.Fatal("a store sized for another firmware build was kept")
		}
	})
	for name, template := range map[string]*string{"template missing on disk": nil, "no template named": new(string)} {
		t.Run(name, func(t *testing.T) {
			spec, tmpl := x86VarsSpec(t, nil)
			if template != nil {
				spec.FirmwareVarsTemplate, tmpl = "", ""
			}
			err := ensureVarsStore(spec)
			if err == nil {
				t.Fatal("no error without a usable template")
			}
			if tmpl != "" && !strings.Contains(err.Error(), tmpl) {
				t.Errorf("error %q does not name the template %s", err, tmpl)
			}
			if _, statErr := os.Stat(spec.FirmwareVars); !os.IsNotExist(statErr) {
				t.Errorf("a store was written without a template: %v", statErr)
			}
		})
	}
}
