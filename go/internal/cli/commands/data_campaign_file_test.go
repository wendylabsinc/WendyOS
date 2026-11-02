package commands

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const pickerCampaign = "version: 1\nname: %s\n"

func writePickerFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCampaignDeployFilePickerOffersOnlyCampaigns(t *testing.T) {
	t.Chdir(t.TempDir())
	writePickerFiles(t, map[string]string{
		"people.yaml":          fmt.Sprintf(pickerCampaign, "people"),
		"doors.yml":            "kind: collection\n" + fmt.Sprintf(pickerCampaign, "doors"),
		"night.YAML":           fmt.Sprintf(pickerCampaign, "night"),
		"notes.txt":            fmt.Sprintf(pickerCampaign, "notes"),
		"compose.yml":          "services:\n  web:\n    image: nginx\n",
		"build.stagefile.yaml": "version: 1\nstages: []\n",
		"empty.yaml":           "",
		"front.watch.yaml":     "kind: watch\nversion: 1\nname: front\n",
	})
	if err := os.Mkdir("nested.yaml", 0o700); err != nil {
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
	writePickerFiles(t, map[string]string{"compose.yml": "services: {}\n"})
	_, err = resolveCampaignDeployFile(nil, true, neverPick)
	if err == nil || !strings.Contains(err.Error(), "no campaign files") || !strings.Contains(err.Error(), "compose.yml") {
		t.Fatalf("no-campaign guidance = %v", err)
	}
	writePickerFiles(t, map[string]string{"watch.yaml": "kind: watch\nversion: 1\nname: w\n"})
	_, err = resolveCampaignDeployFile(nil, true, neverPick)
	if err == nil || !strings.Contains(err.Error(), `watch.yaml (unknown kind "watch"`) {
		t.Fatalf("kind reason missing = %v", err)
	}
	if !strings.Contains(err.Error(), "compose.yml (") {
		t.Fatalf("compose reason missing = %v", err)
	}
	writePickerFiles(t, map[string]string{"people.yml": fmt.Sprintf(pickerCampaign, "people")})
	_, err = resolveCampaignDeployFile(nil, false, neverPick)
	if err == nil || !strings.Contains(err.Error(), "non-interactive or --json") || !strings.Contains(err.Error(), "people.yml") || strings.Contains(err.Error(), "compose.yml") {
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

func TestCampaignDeployErrorExplainsKindOnOldAgent(t *testing.T) {
	old := status.Error(codes.InvalidArgument, "parsing campaign YAML: yaml: unmarshal errors:\n  line 1: field kind not found in type data.Campaign")
	err := campaignDeployError(old)
	if err == nil || !strings.Contains(err.Error(), "wendy device update") || !strings.Contains(err.Error(), "remove the kind line") {
		t.Fatalf("old-agent error = %v", err)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("the gRPC status was lost: %v", status.Code(err))
	}
	other := status.Error(codes.InvalidArgument, "campaign must define at least one source")
	if got := campaignDeployError(other); got != other {
		t.Fatalf("an unrelated error was rewritten: %v", got)
	}
	if campaignDeployError(nil) != nil {
		t.Fatal("nil error rewritten")
	}
}
