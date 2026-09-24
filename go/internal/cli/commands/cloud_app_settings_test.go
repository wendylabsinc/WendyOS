package commands

import (
	"testing"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func TestAppSettingsScope(t *testing.T) {
	t.Run("defaults to authenticated organization", func(t *testing.T) {
		scope, err := appSettingsScope(&appSettingsFlags{}, 7)
		if err != nil {
			t.Fatal(err)
		}
		if got := scope.GetOrganization().GetId(); got != 7 {
			t.Fatalf("organization = %d, want 7", got)
		}
	})

	t.Run("uses exact device", func(t *testing.T) {
		scope, err := appSettingsScope(&appSettingsFlags{device: 42}, 7)
		if err != nil {
			t.Fatal(err)
		}
		if got := scope.GetDevice().GetId(); got != 42 {
			t.Fatalf("device = %d, want 42", got)
		}
	})

	t.Run("rejects two scopes", func(t *testing.T) {
		_, err := appSettingsScope(&appSettingsFlags{organization: 7, device: 42}, 7)
		if err == nil {
			t.Fatal("expected mutually exclusive scope error")
		}
	})
}

func TestAppSettingsChangesUsesControlTypes(t *testing.T) {
	document := &cloudpb.AppSettings{Controls: []*cloudpb.AppSettingsControl{
		{
			Key: "enabled",
			Control: &cloudpb.AppSettingsControl_Toggle{
				Toggle: &cloudpb.AppSettingsControlToggle{},
			},
		},
		{
			Key: "model",
			Control: &cloudpb.AppSettingsControl_SingleSelect{
				SingleSelect: &cloudpb.AppSettingsControlSingleSelect{
					Options: []*cloudpb.AppSettingsControlSelectOption{{Key: "fast"}, {Key: "accurate"}},
				},
			},
		},
		{
			Key: "classes",
			Control: &cloudpb.AppSettingsControl_MultiSelect{
				MultiSelect: &cloudpb.AppSettingsControlMultiSelect{
					Options: []*cloudpb.AppSettingsControlSelectOption{{Key: "fire"}, {Key: "smoke"}},
				},
			},
		},
		{
			Key: "threshold",
			Control: &cloudpb.AppSettingsControl_Slider{
				Slider: &cloudpb.AppSettingsControlSlider{Minimum: 0, Maximum: 1},
			},
		},
	}}

	changes, err := appSettingsChanges(document, []string{
		"enabled=false",
		"model=accurate",
		"classes=fire,smoke",
		"threshold=0.75",
	})
	if err != nil {
		t.Fatal(err)
	}
	if changes["enabled"].GetToggle() {
		t.Fatal("enabled = true, want false")
	}
	if got := changes["model"].GetSingleSelect(); got != "accurate" {
		t.Fatalf("model = %q, want accurate", got)
	}
	if got := changes["classes"].GetMultiSelect().GetKeys(); len(got) != 2 {
		t.Fatalf("classes = %v, want two values", got)
	}
	if got := changes["threshold"].GetSlider(); got != 0.75 {
		t.Fatalf("threshold = %v, want 0.75", got)
	}
}

func TestAppSettingsChangesRejectsInvalidAssignment(t *testing.T) {
	document := &cloudpb.AppSettings{Controls: []*cloudpb.AppSettingsControl{
		{
			Key: "enabled",
			Control: &cloudpb.AppSettingsControl_Toggle{
				Toggle: &cloudpb.AppSettingsControlToggle{},
			},
		},
	}}
	if _, err := appSettingsChanges(document, []string{"enabled=maybe"}); err == nil {
		t.Fatal("expected invalid toggle error")
	}
	if _, err := appSettingsChanges(document, []string{"unknown=value"}); err == nil {
		t.Fatal("expected unknown control error")
	}
}
