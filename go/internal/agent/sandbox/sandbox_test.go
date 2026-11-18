package sandbox

import (
	"os"
	"os/exec"
	"testing"
)

// Without root (and off Linux) there is nothing to drop: Lookup yields no
// identity, and starting through it is a plain Start.
func TestNoIdentityStartsTheCommandUnchanged(t *testing.T) {
	id, err := Lookup("nobody", "video")
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && id != nil {
		t.Fatalf("unprivileged process resolved an identity: %v", id)
	}
	var none *Identity
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := none.Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil || cmd.Dir != "" || cmd.Env != nil {
		t.Fatalf("command was changed: attr=%v dir=%q env=%v", cmd.SysProcAttr, cmd.Dir, cmd.Env)
	}
}
