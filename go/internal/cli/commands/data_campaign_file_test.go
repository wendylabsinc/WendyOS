package commands

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestCampaignDeployFilePicker(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"people.yaml", "doors.yml", "night.YAML", "notes.txt"} {
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir("nested.yaml", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("nested.yaml/hidden.yml", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := resolveCampaignDeployFile(nil, true, func(_ string, items []tui.PickerItem) (string, error) {
		var names []string
		for _, item := range items {
			names = append(names, item.Value.(string))
		}
		if !reflect.DeepEqual(names, []string{"doors.yml", "night.YAML", "people.yaml"}) {
			t.Fatalf("picker files = %v", names)
		}
		return items[2].Value.(string), nil
	})
	if err != nil || path != "people.yaml" {
		t.Fatalf("selected %q: %v", path, err)
	}
	_, err = resolveCampaignDeployFile(nil, true, func(string, []tui.PickerItem) (string, error) { return "", ErrUserCancelled })
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestCampaignDeployFileMissingPathGuidance(t *testing.T) {
	t.Chdir(t.TempDir())
	neverPick := func(string, []tui.PickerItem) (string, error) { t.Fatal("unexpected picker"); return "", nil }
	_, err := resolveCampaignDeployFile(nil, true, neverPick)
	if err == nil || !strings.Contains(err.Error(), "no .yaml or .yml files") || !strings.Contains(err.Error(), "wendy data campaign deploy campaign.yaml") {
		t.Fatalf("missing file guidance = %v", err)
	}
	if err := os.WriteFile("people.yml", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = resolveCampaignDeployFile(nil, false, neverPick)
	if err == nil || !strings.Contains(err.Error(), "non-interactive or --json") || !strings.Contains(err.Error(), "people.yml") {
		t.Fatalf("non-interactive guidance = %v", err)
	}
	path, err := resolveCampaignDeployFile([]string{"../explicit.yaml"}, false, neverPick)
	if err != nil || path != "../explicit.yaml" {
		t.Fatalf("explicit path = %q: %v", path, err)
	}
	cmd := newDataCampaignDeployCmd()
	if err := cmd.ValidateArgs(nil); err != nil {
		t.Fatalf("picker blocked by argument validation: %v", err)
	}
	if err := cmd.ValidateArgs([]string{"one.yaml", "two.yml"}); err == nil {
		t.Fatal("accepted multiple paths")
	}
}
