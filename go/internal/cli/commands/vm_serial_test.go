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
	c := vm.SerialConfig{Port: "COM12", Baud: 1000000}
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
	var got vm.SerialConfig
	if err := json.Unmarshal([]byte(out), &got); err != nil || got != c {
		t.Fatalf("inspect: %q %v", out, err)
	}
	if _, err := run("serial", "serial", "--clear", "--port", "COM12"); err == nil {
		t.Fatal("accepted conflicting flags")
	}
	if _, err := run("serial", "serial", "--clear"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("serial", "serial"); err != nil || strings.TrimSpace(out) != "null" {
		t.Fatalf("clear: %q %v", out, err)
	}
	_, err = run("serial", "serial", "--port", "com12", "--baud", "1000000")
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ReadSerial("serial")
		if err != nil || got == nil || *got != c {
			t.Fatalf("set: %v %v", got, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("unsupported host: %v", err)
	}
}
