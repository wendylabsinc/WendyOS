//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func seedListOrgsConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	t.Setenv("WENDY_SECRET_STORE", "file")
	cfg := &config.Config{
		Auth:              []config.AuthConfig{v2Session("audit.invalid:443")},
		DefaultCloudGRPC:  "audit.invalid:443",
		DefaultTenantUUID: "org-a",
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
		} else if !bytes.Equal(before, after) {
			t.Error("listing organizations modified the stored configuration")
		}
	})
}

func TestAuthListOrgsOutputModes(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		json, interactive, empty bool
	}{
		{name: "explicit JSON in terminal", json: true, interactive: true},
		{name: "noninteractive without explicit JSON"},
		{name: "noninteractive JSON", json: true},
		{name: "empty JSON membership", json: true, interactive: true, empty: true},
		{name: "empty noninteractive membership", empty: true},
		{name: "interactive management", interactive: true},
		{name: "empty interactive membership", interactive: true, empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedListOrgsConfig(t)
			oldJSON, oldInteractive, oldPicker := jsonOutput, isInteractiveTerminalFn, pickCloudOrgV2
			t.Cleanup(func() {
				jsonOutput, isInteractiveTerminalFn, pickCloudOrgV2 = oldJSON, oldInteractive, oldPicker
			})
			jsonOutput = tc.json
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			stubListOrgsV2(t, func(*config.AuthConfig) ([]*pb.Organization, error) {
				if tc.empty {
					return nil, nil
				}
				return []*pb.Organization{v2Org("org-a", "Audit"), v2Org("org-b", "Second")}, nil
			})
			pickerCalled := false
			pickCloudOrgV2 = func(orgs []*pb.Organization, _ *config.AuthConfig, _ *config.Config, management bool) (string, error) {
				pickerCalled = true
				if !management || len(orgs) != 2 {
					t.Error("interactive call did not preserve the management picker")
				}
				return "", ErrUserCancelled
			}
			var stdout, stderr bytes.Buffer
			cmd := newAuthListOrgsCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(nil)
			err := cmd.Execute()
			if tc.interactive && !tc.json && !tc.empty {
				if !pickerCalled || !errors.Is(err, ErrUserCancelled) {
					t.Fatalf("picker called=%v, err=%v", pickerCalled, err)
				}
				return
			}
			if err != nil || pickerCalled {
				t.Fatalf("listing failed: picker called=%v, err=%v", pickerCalled, err)
			}
			if tc.interactive && !tc.json {
				if stdout.String() != "Your account belongs to no organizations.\n" {
					t.Fatalf("unexpected interactive output: %q", stdout.String())
				}
				return
			}
			var got []map[string]string
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("invalid JSON: %v; output=%q", err, stdout.String())
			}
			want := []map[string]string{}
			if !tc.empty {
				want = []map[string]string{{"id": "org-a", "name": "Audit"}, {"id": "org-b", "name": "Second"}}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got, want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("unexpected stderr: %s", stderr.String())
			}
		})
	}
}

func TestAuthListOrgsJSONPropagatesLookupFailure(t *testing.T) {
	seedListOrgsConfig(t)
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })
	lookupErr := errors.New("organization service unavailable")
	stubListOrgsV2(t, func(*config.AuthConfig) ([]*pb.Organization, error) { return nil, lookupErr })
	var stdout, stderr bytes.Buffer
	cmd := newAuthListOrgsCmd()
	// Match the root command's error/usage policy when executing this child alone.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if !errors.Is(err, lookupErr) || !strings.Contains(stderr.String(), lookupErr.Error()) {
		t.Fatalf("lookup failure lost: err=%v, stderr=%q", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed lookup emitted misleading output: %q", stdout.String())
	}
}
