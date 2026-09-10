package commands

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
)

func runDataEnroll(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newDataEnrollCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func readStaged(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, pkienroll.StagedFileName))
	if err != nil {
		t.Fatalf("reading staged file: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding staged file: %v", err)
	}
	return got
}

func TestDataEnrollStagesTheCredential(t *testing.T) {
	dir := t.TempDir()
	out, err := runDataEnroll(t,
		"--tenant", "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		"--token", "tok-abc",
		"--device-id", "sh/wendy/2/408",
		"--environment", "dev",
		"--config-path", dir,
	)
	if err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}

	got := readStaged(t, dir)
	if got["token"] != "tok-abc" {
		t.Errorf("token = %q", got["token"])
	}
	if got["tenantUUID"] != "022b7284-f7f3-4d86-b844-d105a7c06d9e" {
		t.Errorf("tenantUUID = %q", got["tenantUUID"])
	}
	if got["deviceID"] != "sh/wendy/2/408" {
		t.Errorf("deviceID = %q", got["deviceID"])
	}
	if got["environment"] != "dev" {
		t.Errorf("environment = %q", got["environment"])
	}
	// Optional fields that were not given must be absent rather than empty,
	// so the agent's own defaulting decides them.
	if _, present := got["csrEndpoint"]; present {
		t.Error("csrEndpoint was written although it was not given")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, pkienroll.StagedFileName))
		if err != nil {
			t.Fatalf("stat staged file: %v", err)
		}
		// It holds a bearer credential.
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %v, want 0600", got)
		}
	}
}

func TestDataEnrollTakesTheTenantFromTheTokenClaim(t *testing.T) {
	dir := t.TempDir()
	enc := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(map[string]any{"type": "asset_enrollment", "org_id": 2, "asset_id": 408,
		"tenant_uuid": "022b7284-f7f3-4d86-b844-d105a7c06d9e"})
	if err != nil {
		t.Fatalf("encoding claims: %v", err)
	}
	token := enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"

	if out, err := runDataEnroll(t, "--token", token, "--config-path", dir); err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}
	if got := readStaged(t, dir)["tenantUUID"]; got != "022b7284-f7f3-4d86-b844-d105a7c06d9e" {
		t.Errorf("tenantUUID = %q, want the token's claim", got)
	}
}

func TestDataEnrollRequiresATenantForAnOpaqueToken(t *testing.T) {
	// pki-core's own enrollment tokens are opaque and carry no claims, so the
	// tenant has to be given. Refusing here is better than staging a file the
	// agent will reject at startup.
	dir := t.TempDir()
	_, err := runDataEnroll(t, "--token", "opaque-value", "--config-path", dir)
	if err == nil {
		t.Fatal("want an error naming --tenant")
	}
	if _, statErr := os.Stat(filepath.Join(dir, pkienroll.StagedFileName)); !os.IsNotExist(statErr) {
		t.Error("a file was staged despite the refusal")
	}
}

func TestDataEnrollRequiresAToken(t *testing.T) {
	if _, err := runDataEnroll(t, "--tenant", "t", "--config-path", t.TempDir()); err == nil {
		t.Fatal("want an error naming --token")
	}
}

func TestAgentConfigPathFollowsTheAgent(t *testing.T) {
	t.Setenv("WENDY_CONFIG_PATH", "")
	if got := agentConfigPath(); got != defaultAgentConfigPath {
		t.Errorf("agentConfigPath = %q, want %q", got, defaultAgentConfigPath)
	}
	t.Setenv("WENDY_CONFIG_PATH", "/tmp/wendy-scratch")
	if got := agentConfigPath(); got != "/tmp/wendy-scratch" {
		t.Errorf("agentConfigPath = %q, want the override", got)
	}
}
