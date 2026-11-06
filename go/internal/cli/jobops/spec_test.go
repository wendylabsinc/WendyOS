package jobops_test

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecFileAndInlineHaveIdenticalStrictValidation(t *testing.T) {
	spec := jobops.Spec{Name: "file-spec", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "check", Argv: []string{"python3", "-c", "print('ok')"}, TimeoutSeconds: 2}}}
	data, _ := json.Marshal(spec)
	name := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := jobops.LoadSpec(name)
	encoded, _ := json.Marshal(loaded)
	if err != nil || string(encoded) != string(data) {
		t.Fatalf("file spec changed: %v %s", err, encoded)
	}
	for _, bad := range [][]byte{append(append([]byte{}, data...), []byte(" {}")...), []byte(`{"name":"x","unexpected":true}`), []byte("null"), []byte(strings.Repeat(" ", 120001))} {
		if _, err = jobops.ParseSpec(bad); err == nil {
			t.Fatalf("accepted invalid spec: %.100s", bad)
		}
		os.WriteFile(name, bad, 0600)
		if _, err = jobops.LoadSpec(name); err == nil {
			t.Fatal("file validation diverges from inline")
		}
	}
	if _, err = jobops.LoadSpec(filepath.Dir(name)); err == nil {
		t.Fatal("read directory as spec")
	}
	link := name + "-link"
	if os.Symlink(name, link) == nil {
		if _, err = jobops.LoadSpec(link); err == nil {
			t.Fatal("read symlink as spec")
		}
	}
}

func TestSpecFIFORefusedWithoutBlocking(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	name := filepath.Join(t.TempDir(), "spec.fifo")
	if err = exec.Command(python, "-c", "import os,sys;os.mkfifo(sys.argv[1])", name).Run(); err != nil {
		t.Skip(err)
	}
	if _, err = jobops.LoadSpec(name); err == nil {
		t.Fatal("accepted FIFO")
	}
}
