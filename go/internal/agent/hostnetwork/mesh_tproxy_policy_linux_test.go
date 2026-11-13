//go:build linux

package hostnetwork

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeshUDPPolicyForeignStateAndCleanupRetryNamespace(t *testing.T) {
	if os.Getenv("WENDY_TEST_NETNS") != "1" {
		t.Skip("disposable privileged namespace only")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
		return string(out)
	}
	for _, foreign := range []string{"rule", "route"} {
		t.Run(foreign, func(t *testing.T) {
			t.Cleanup(func() {
				exec.Command("ip", "-4", "rule", "del", "pref", meshUDPPriority).Run()
				exec.Command("ip", "-4", "route", "flush", "table", meshUDPTable).Run()
			})
			args := []string{"-4", "route", "add", "local", "default", "dev", "lo", "table", meshUDPTable}
			if foreign == "route" {
				args = append(args, "src", "127.0.0.2")
			}
			run(args...)
			args = []string{"-4", "rule", "add", "pref", meshUDPPriority, "fwmark", meshUDPMark, "lookup", meshUDPTable}
			if foreign == "rule" {
				args = append(args, "from", "192.168.1.0/24")
			}
			run(args...)
			rules := run("-4", "rule", "show", "pref", meshUDPPriority)
			routes := run("-4", "route", "show", "table", meshUDPTable)
			if err := InitMeshUDPTProxy(50059); err == nil {
				t.Error("adopted foreign policy at initialization")
			}
			if err := CloseMeshUDPTProxy(); err == nil {
				t.Error("cleanup accepted foreign policy")
			}
			if got := run("-4", "rule", "show", "pref", meshUDPPriority); got != rules {
				t.Errorf("foreign rule changed: %q -> %q", rules, got)
			}
			if got := run("-4", "route", "show", "table", meshUDPTable); got != routes {
				t.Errorf("foreign route changed: %q -> %q", routes, got)
			}
		})
	}
	if err := InitMeshUDPTProxy(50059); err != nil {
		t.Fatal(err)
	}
	realIP, err := exec.LookPath("ip")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "fail-delete")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
'-4 route del local '*) if test -f "$WENDY_UDP_FAIL_ROUTE_DELETE"; then exit 2; fi;;
esac
exec "$WENDY_UDP_REAL_IP" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_UDP_REAL_IP", realIP)
	t.Setenv("WENDY_UDP_FAIL_ROUTE_DELETE", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := CloseMeshUDPTProxy(); err == nil {
		t.Fatal("injected policy deletion failure hidden")
	}
	if got := run("-4", "route", "show", "table", meshUDPTable); !strings.Contains(got, "local default") {
		t.Fatalf("failed deletion lost retry state: %s", got)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := CloseMeshUDPTProxy(); err != nil {
		t.Fatalf("cleanup retry after chain deletion: %v", err)
	}
	if err := CloseMeshUDPTProxy(); err != nil {
		t.Fatalf("absent cleanup not idempotent: %v", err)
	}
}
