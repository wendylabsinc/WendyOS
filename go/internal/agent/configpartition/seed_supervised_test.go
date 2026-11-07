package configpartition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/supervisor"
	"go.uber.org/zap"
)

func TestStageSeedHandsTheBinaryToTheSupervisor(t *testing.T) {
	dir, inbox := t.TempDir(), t.TempDir()
	data, err := os.ReadFile(makeELF(t, 2, elfMachineByArch[runtime.GOARCH]))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "wendy-agent")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	if !stageSeedForSupervisor(zap.NewNop(), dir, inbox, t.TempDir()) {
		t.Fatal("want the seed staged")
	}
	staged := filepath.Join(inbox, sha)
	if got, err := os.ReadFile(staged); err != nil || string(got) != string(data) {
		t.Fatalf("the seed must be in the inbox under its sha256: %v", err)
	}
	var meta supervisor.Incoming
	raw, err := os.ReadFile(staged + ".json")
	if err != nil || json.Unmarshal(raw, &meta) != nil || meta.SHA256 != sha || meta.RemoteVia != "" || !meta.Seed {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("the seed must be removed once staged")
	}
}

func TestStageSeedDropsANonELF(t *testing.T) {
	dir, inbox := t.TempDir(), t.TempDir()
	src := filepath.Join(dir, "wendy-agent")
	os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755)
	if stageSeedForSupervisor(zap.NewNop(), dir, inbox, t.TempDir()) {
		t.Fatal("a non-ELF seed must not be staged")
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
		t.Fatal("nothing may reach the inbox")
	}
}

func TestStageSeedWaitsWhileUpdatesArePaused(t *testing.T) {
	dir, inbox := t.TempDir(), t.TempDir()
	data, _ := os.ReadFile(makeELF(t, 2, elfMachineByArch[runtime.GOARCH]))
	src := filepath.Join(dir, "wendy-agent")
	os.WriteFile(src, data, 0o644)
	statusPath := filepath.Join(t.TempDir(), "status.json")
	raw, _ := json.Marshal(supervisor.Status{UpdatesPaused: "agent updates are off: /data is not mounted"})
	os.WriteFile(statusPath, raw, 0o644)
	orig := supervisorStatusPath
	supervisorStatusPath = statusPath
	t.Cleanup(func() { supervisorStatusPath = orig })

	if stageSeedForSupervisor(zap.NewNop(), dir, inbox, t.TempDir()) {
		t.Fatal("the seed must wait while updates are paused")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("the seed must be kept for later")
	}
}

func TestStageSeedWaitsForFirstBootEnrollment(t *testing.T) {
	dir, inbox := t.TempDir(), t.TempDir()
	data, _ := os.ReadFile(makeELF(t, 2, elfMachineByArch[runtime.GOARCH]))
	src := filepath.Join(dir, "wendy-agent")
	os.WriteFile(src, data, 0o644)
	os.WriteFile(filepath.Join(dir, "acme-enrollment.json"), []byte("{}"), 0o600)
	if stageSeedForSupervisor(zap.NewNop(), dir, inbox, t.TempDir()) {
		t.Fatal("a seed must not take over before the image agent has enrolled the device")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("the seed must be kept for a later start")
	}
}

func seedFixture(t *testing.T) (cfgDir, inbox, src string) {
	t.Helper()
	cfgDir, inbox = t.TempDir(), t.TempDir()
	data, _ := os.ReadFile(makeELF(t, 2, elfMachineByArch[runtime.GOARCH]))
	src = filepath.Join(cfgDir, "wendy-agent")
	os.WriteFile(src, data, 0o644)
	return cfgDir, inbox, src
}

func statusForTest(t *testing.T, st supervisor.Status) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status.json")
	raw, _ := json.Marshal(st)
	os.WriteFile(path, raw, 0o644)
	orig := supervisorStatusPath
	supervisorStatusPath = path
	t.Cleanup(func() { supervisorStatusPath = orig })
}

func TestStageSeedDropsTheSeedOnceAnAgentRunsFromData(t *testing.T) {
	cfgDir, inbox, src := seedFixture(t)
	statusForTest(t, supervisor.Status{Role: supervisor.RoleApproved})
	if stageSeedForSupervisor(zap.NewNop(), cfgDir, inbox, t.TempDir()) {
		t.Fatal("a seed must never replace an agent chosen since")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("the stale seed must be removed")
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
		t.Fatal("nothing may reach the inbox")
	}
}

func TestStageSeedStagesOnceEnrollmentHasBeenTried(t *testing.T) {
	cases := map[string]string{
		"attempt marker": "acme-first-boot-attempt.json",
		"provisioned":    "provisioning.json",
		"legacy handoff": "enrollment.json",
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			cfgDir, inbox, _ := seedFixture(t)
			configPath := t.TempDir()
			os.WriteFile(filepath.Join(cfgDir, "acme-enrollment.json"), []byte("{}"), 0o600)
			os.WriteFile(filepath.Join(configPath, file), []byte("{}"), 0o600)
			if !stageSeedForSupervisor(zap.NewNop(), cfgDir, inbox, configPath) {
				t.Fatal("once enrollment can no longer use the credential, the seed must go ahead")
			}
		})
	}
}

func TestSeedWaitingForTheSupervisor(t *testing.T) {
	cfgDir, _, _ := seedFixture(t)
	configPath := t.TempDir()
	t.Setenv(supervisor.EnvSupervisor, "/opt/wendyos/bin/wendy-agent-supervisor")
	t.Setenv(supervisor.EnvStore, t.TempDir())
	os.WriteFile(filepath.Join(cfgDir, "acme-enrollment.json"), []byte("{}"), 0o600)
	if seedWaiting(cfgDir, configPath) {
		t.Fatal("the seed still waits for enrollment")
	}
	os.WriteFile(filepath.Join(configPath, "acme-first-boot-attempt.json"), []byte("{}"), 0o600)
	if !seedWaiting(cfgDir, configPath) {
		t.Fatal("after the enrollment attempt the seed is ready to stage")
	}
	t.Setenv(supervisor.EnvSupervisor, "")
	if seedWaiting(cfgDir, configPath) {
		t.Fatal("without a supervisor the seed is installed the old way")
	}
}

func TestStageSeedHandsOverALocalBinaryAsTheUsersChoice(t *testing.T) {
	dir, inbox := t.TempDir(), t.TempDir()
	data, err := os.ReadFile(makeELF(t, 2, elfMachineByArch[runtime.GOARCH]))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "wendy-agent"), data, 0o644)
	os.WriteFile(filepath.Join(dir, "wendy-agent.local"), nil, 0o644)
	if !stageSeedForSupervisor(zap.NewNop(), dir, inbox, t.TempDir()) {
		t.Fatal("want the binary staged")
	}
	sum := sha256.Sum256(data)
	var meta supervisor.Incoming
	raw, err := os.ReadFile(filepath.Join(inbox, hex.EncodeToString(sum[:])+".json"))
	if err != nil || json.Unmarshal(raw, &meta) != nil || meta.Seed {
		t.Fatalf("a --binary always gets its trial: %+v, %v", meta, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wendy-agent.local")); !os.IsNotExist(err) {
		t.Fatal("the marker goes with the binary")
	}
}

func TestStageSeedDoesNotRestartForASeedItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove it anyway")
	}
	dir, inbox := t.TempDir(), t.TempDir()
	data, err := os.ReadFile(makeELF(t, 2, elfMachineByArch[runtime.GOARCH]))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "wendy-agent"), data, 0o644)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	// Staged again on every start, it would restart the agent in a loop.
	if stageSeedForSupervisor(zap.NewNop(), dir, inbox, t.TempDir()) {
		t.Fatal("a seed that stays on /config must not restart the agent")
	}
}
