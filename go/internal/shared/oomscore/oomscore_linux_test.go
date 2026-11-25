package oomscore

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestResetChildInKernelDoesNotChangeParent(t *testing.T) {
	before, err := os.ReadFile(procScorePath)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := strconv.Atoi(strings.TrimSpace(string(before)))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/cat", procScorePath)
	ResetChild(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v; %s", err, out)
	}
	if want := strconv.Itoa(max(parent, 0)); strings.TrimSpace(string(out)) != want {
		t.Fatalf("child score = %q; want %s", out, want)
	}
	after, err := os.ReadFile(procScorePath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("parent score changed from %q to %q; err %v", before, after, err)
	}
}
