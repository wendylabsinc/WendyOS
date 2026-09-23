//go:build linux

package localmesh

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakePolicyCommands(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	failed := filepath.Join(dir, "failed-once")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$WENDY_TEST_IPTABLES_LOG"
case "$*" in
  *"-j FAIL_ONCE"*)
    if [ ! -e "$WENDY_TEST_IPTABLES_FAILED" ]; then
      : > "$WENDY_TEST_IPTABLES_FAILED"
      echo 'xtables lock is held' >&2
      exit 4
    fi
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_TEST_IPTABLES_LOG", log)
	t.Setenv("WENDY_TEST_IPTABLES_FAILED", failed)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func testPolicyRules() []policyRule {
	return []policyRule{
		{table: "filter", chain: "FORWARD", args: []string{"-j", "FIRST"}},
		{table: "filter", chain: "FORWARD", args: []string{"-j", "FAIL_ONCE"}},
		{table: "nat", chain: "POSTROUTING", args: []string{"-j", "LAST"}},
	}
}

func checkDeletedRules(t *testing.T, log string) {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.Contains(line, "-j FIRST"):
			got = append(got, "FIRST")
		case strings.Contains(line, "-j FAIL_ONCE"):
			got = append(got, "FAIL_ONCE")
		case strings.Contains(line, "-j LAST"):
			got = append(got, "LAST")
		default:
			t.Fatalf("unexpected iptables command: %q", line)
		}
	}
	want := []string{"LAST", "FAIL_ONCE", "FIRST", "FAIL_ONCE"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deletion order = %v, want %v", got, want)
	}
}

func TestHostPolicyStopSharingRetriesOnlyFailedRule(t *testing.T) {
	log := fakePolicyCommands(t)
	p := &HostPolicy{share: testPolicyRules(), sharing: "eth0|1.1.1.1"}
	if err := p.stopSharing(context.Background()); err == nil {
		t.Fatal("expected transient deletion failure")
	}
	if p.sharing != "eth0|1.1.1.1" || len(p.share) != 1 || p.share[0].args[1] != "FAIL_ONCE" {
		t.Fatalf("lost ownership after partial failure: sharing=%q rules=%v", p.sharing, p.share)
	}
	if err := p.SetSharing(context.Background(), "", ""); err != nil {
		t.Fatalf("retry withdrawal: %v", err)
	}
	if p.sharing != "" || len(p.share) != 0 {
		t.Fatalf("withdrawal incomplete: sharing=%q rules=%v", p.sharing, p.share)
	}
	checkDeletedRules(t, log)
}

func TestHostPolicyCloseRetriesOnlyFailedBaseRule(t *testing.T) {
	log := fakePolicyCommands(t)
	p := &HostPolicy{base: testPolicyRules()}
	if err := p.Close(); err == nil {
		t.Fatal("expected transient deletion failure")
	}
	if len(p.base) != 1 || p.base[0].args[1] != "FAIL_ONCE" {
		t.Fatalf("lost base ownership after partial failure: %v", p.base)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if len(p.base) != 0 {
		t.Fatalf("base rules remain after retry: %v", p.base)
	}
	checkDeletedRules(t, log)
}

func TestHostPolicySetSharingRetriesResidualAfterFailedSetup(t *testing.T) {
	log := fakePolicyCommands(t)
	p := &HostPolicy{share: testPolicyRules()}
	if err := p.stopSharing(context.Background()); err == nil {
		t.Fatal("expected transient deletion failure")
	}
	if err := p.SetSharing(context.Background(), "", ""); err != nil {
		t.Fatalf("retry residual rules with unchanged desired state: %v", err)
	}
	if len(p.share) != 0 {
		t.Fatalf("residual sharing rules remain: %v", p.share)
	}
	checkDeletedRules(t, log)
}
