//go:build linux

package localmesh

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostDNSPolicyCleanupAcrossMountNamespace(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux container")
	}
	cmd := exec.Command("unshare", "--mount", "--propagation", "private", os.Args[0], "-test.run=^TestHostDNSPolicyCleanupProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "WENDY_DNS_POLICY_PROCESS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("DNS cleanup: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

// Only this subprocess sees the temporary /etc and /run. Exercise the real
// resolver symlink and command paths without changing the test host's DNS.
func TestHostDNSPolicyCleanupProcess(t *testing.T) {
	if os.Getenv("WENDY_DNS_POLICY_PROCESS") != "1" {
		t.Skip("mount namespace subprocess")
	}
	for _, path := range []string{"/etc", "/run"} {
		if err := unix.Mount("tmpfs", path, "tmpfs", 0, "mode=0755"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(resolverStub), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolverStub, []byte("nameserver 127.0.0.53\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$WENDY_DNS_POLICY_LOG\"\nif [ -e \"$WENDY_DNS_POLICY_FAILURES/$1\" ]; then exit 1; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "resolvectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx := context.Background()
	for _, phase := range []string{"resolver restoration", "partial DNS apply", "partial domain apply", "process restart", "checkpoint before replacement"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			original := filepath.Join(dir, "original-resolv.conf")
			if err := os.WriteFile(original, []byte("nameserver 192.0.2.53\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove("/etc/resolv.conf")
			if err := os.Symlink(original, "/etc/resolv.conf"); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "commands")
			if err := os.WriteFile(log, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WENDY_DNS_POLICY_LOG", log)
			t.Setenv("WENDY_DNS_POLICY_FAILURES", dir)
			policy := &HostPolicy{}
			if phase == "checkpoint before replacement" {
				data, err := json.Marshal(resolverJournal{Version: 1, Previous: original})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(policy.files().journal, data, 0600); err != nil {
					t.Fatal(err)
				}
			} else if phase == "process restart" {
				if err := policy.SetDNS(ctx, "lo", "fd45:b891:567f::1"); err != nil {
					t.Fatal(err)
				}
				policy = &HostPolicy{} // the original process lost its in-memory state
			} else if phase == "resolver restoration" {
				if err := policy.SetDNS(ctx, "lo", "fd45:b891:567f::1"); err != nil {
					t.Fatal(err)
				}
				// An external replacement makes the first restore fail safely.
				if err := replaceResolverLink("/etc/resolv.conf", "/externally-replaced"); err != nil {
					t.Fatal(err)
				}
				if err := policy.SetDNS(ctx, "", ""); err == nil {
					t.Fatal("external resolver ownership change was ignored")
				}
				if err := replaceResolverLink("/etc/resolv.conf", resolverStub); err != nil {
					t.Fatal(err)
				}
			} else {
				stage := "dns"
				if phase == "partial domain apply" {
					stage = "domain"
				}
				for _, name := range []string{stage, "revert"} {
					if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := policy.SetDNS(ctx, "lo", "fd45:b891:567f::1"); err == nil {
					t.Fatal("injected apply failure was ignored")
				}
				for _, name := range []string{stage, "revert"} {
					if err := os.Remove(filepath.Join(dir, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if err := policy.Close(); err != nil {
				t.Fatal(err)
			}
			if target, err := os.Readlink("/etc/resolv.conf"); err != nil || target != original {
				t.Errorf("cleanup retry did not restore original resolver: %q %v", target, err)
			}
			if policy.dnsInterface != "" || policy.dnsAddress != "" || policy.previousResolver != "" {
				t.Errorf("cleanup retry retained ownership: %+v", policy)
			}
			commands, err := os.ReadFile(log)
			if err != nil || ((phase == "resolver restoration" || strings.HasPrefix(phase, "partial")) && !strings.HasSuffix(string(commands), "revert lo\n")) {
				t.Errorf("cleanup retry did not revert owned link: %q %v", commands, err)
			}
			if strings.HasPrefix(phase, "partial") && len(commands) <= len(before) {
				t.Error("cleanup did not retry the failed link revert")
			}
			if _, err := os.Lstat(policy.files().journal); !os.IsNotExist(err) {
				t.Errorf("cleanup retained resolver checkpoint: %v", err)
			}
		})
	}
}
