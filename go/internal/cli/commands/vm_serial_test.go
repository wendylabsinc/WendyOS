package commands

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestVMSerialInspectAndClear(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	s, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFrom("serial", strings.NewReader("disk"), 4, 4096, vm.Meta{}); err != nil {
		t.Fatal(err)
	}
	// The port as a user would type it on this host, and as it is stored.
	typed, stored := " /dev/ttyACM0 ", "/dev/ttyACM0"
	foreign := "COM12"
	if runtime.GOOS == "windows" {
		typed, stored, foreign = "com12", "COM12", "/dev/ttyACM0"
	}
	c := vm.SerialConfig{Port: stored, Baud: 1000000}
	if err := s.ConfigureSerial("serial", &c); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		cmd := newVMCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := run("serial", "serial")
	if err != nil {
		t.Fatal(err)
	}
	var inspected vm.SerialConfig
	if err := json.Unmarshal([]byte(out), &inspected); err != nil || inspected != c {
		t.Fatalf("inspect: %q %v", out, err)
	}
	if _, err := run("serial", "serial", "--clear", "--port", stored); err == nil {
		t.Fatal("accepted conflicting flags")
	}
	if _, err := run("serial", "serial", "--clear"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("serial", "serial"); err != nil || strings.TrimSpace(out) != "null" {
		t.Fatalf("clear: %q %v", out, err)
	}
	if _, err := run("serial", "serial", "--port", foreign, "--baud", "1000000"); err == nil {
		t.Fatalf("accepted the other platform's port name %q", foreign)
	}
	if _, err := run("serial", "serial", "--port", typed, "--baud", "1000000"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSerial("serial")
	if err != nil || got == nil || *got != c {
		t.Fatalf("set: %v %v", got, err)
	}
}
