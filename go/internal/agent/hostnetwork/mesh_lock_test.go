package hostnetwork

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The fake models iptables exiting 4 when another process owns xtables.lock
// unless the caller requests a bounded wait. No root or host netfilter state
// is needed, so the boot-time filter and NAT paths can be checked on CI.
func installLockCheckingIPTables(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "iptables")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != -w ] || [ \"$2\" != 5 ]; then echo 'xtables lock is held' >&2; exit 4; fi\n" +
		"shift 2\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestMeshFilterAndRedirectWaitForXtablesLock(t *testing.T) {
	dir := installLockCheckingIPTables(t,
		"printf '%s\\n' \"$*\" >> \"$MESH_IPTABLES_LOG\"\n"+
			"case \" $* \" in *' -C '*) exit 1;; esac\n")
	log := filepath.Join(dir, "calls")
	t.Setenv("MESH_IPTABLES_LOG", log)
	if err := InitMeshChain(); err != nil {
		t.Fatal(err)
	}
	if err := InitMeshNATChain(); err != nil {
		t.Fatal(err)
	}
	if err := AddMeshRule("10.110.1.2", "10.99.0.0/16"); err != nil {
		t.Fatal(err)
	}
	if err := AddMeshRedirect("10.110.1.2", "10.99.0.0/16", 43024); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-t filter -N WENDY-MESH", "-t filter -C FORWARD", "-t filter -A FORWARD",
		"-t nat -N WENDY-MESH", "-t nat -C PREROUTING", "-t nat -A PREROUTING",
		"-C WENDY-MESH -t filter", "-A WENDY-MESH -t filter",
		"-C WENDY-MESH -t nat", "-A WENDY-MESH -t nat",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("missing iptables call %q from:\n%s", want, calls)
		}
	}
}

func TestConcurrentMeshRuleAddChecksAndAppendsOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  func() error
	}{
		{"filter ACCEPT", func() error { return AddMeshRule("10.110.1.2", "10.99.0.0/16") }},
		{"nat REDIRECT", func() error { return AddMeshRedirect("10.110.1.2", "10.99.0.0/16", 43024) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installLockCheckingIPTables(t,
				"case \"$1\" in\n"+
					"-C) if [ -e \"$MESH_IPTABLES_STATE\" ]; then exit 0; fi; sleep 0.1; exit 1;;\n"+
					"-A) touch \"$MESH_IPTABLES_STATE\"; printf 'add\\n' >> \"$MESH_IPTABLES_LOG\";;\n"+
					"esac\n")
			log := filepath.Join(dir, "calls")
			t.Setenv("MESH_IPTABLES_LOG", log)
			t.Setenv("MESH_IPTABLES_STATE", filepath.Join(dir, "state"))
			const workers = 12
			var wg sync.WaitGroup
			errCh := make(chan error, workers)
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errCh <- tc.add()
				}()
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					t.Fatal(err)
				}
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(calls), "add\n"); got != 1 {
				t.Fatalf("%d concurrent appends; want exactly one", got)
			}
		})
	}
}
