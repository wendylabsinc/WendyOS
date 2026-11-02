package data

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
	"gopkg.in/yaml.v3"
)

// Revisions of the example campaigns on main before data documents
// (fbcbb402a). A file without kind, and the same file with kind: collection,
// must keep them: a moved revision looks like an edited plan to operators and
// to the fleet backend.
var goldenCampaignRevisions = map[string]string{
	"../../../../Examples/WendyDataCampaign/campaign.yaml": "9b52fc99f73319c7f9a6207a5fd242c12ab9ef67ebd9176de7b8ada814446012",
	"../../../../Examples/WendyDataPeople/campaign.yaml":   "5bdd06fff93d5871fd29b77c85a8a9f17d28486b06c2a4af4d21fa610f1ca625",
	"../../../../Examples/WendyDataModelApp/campaign.yaml": "39fda09f79a85b497b1efebf33cae2c83bea8ef61a2d81240e7446e60299a2d5",
}

func TestParseDocumentKeepsCampaignRevisions(t *testing.T) {
	for path, want := range goldenCampaignRevisions {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for label, contents := range map[string][]byte{"without kind": raw, "with kind": append([]byte("kind: collection\n"), raw...)} {
			doc, err := ParseDocument(contents)
			if err != nil {
				t.Fatalf("%s %s: %v", path, label, err)
			}
			campaign, ok := doc.Body.(Campaign)
			if !ok {
				t.Fatalf("%s %s: body is %T, want Campaign", path, label, doc.Body)
			}
			if doc.Header.Kind != datadoc.DefaultKind || doc.Header.Name != campaign.Name || doc.Header.Version != 1 {
				t.Fatalf("%s %s: header = %+v", path, label, doc.Header)
			}
			if doc.Revision != want || campaign.Revision != want {
				t.Fatalf("%s %s: revision %s, want %s", path, label, doc.Revision, want)
			}
		}
	}
}

func TestParseCampaignAcceptsOnlyTheCollectionKind(t *testing.T) {
	base := string(minimalCampaign(1, "telemetry: true", "wifi"))
	if _, err := ParseCampaign([]byte("kind: collection\n" + base)); err != nil {
		t.Fatalf("kind: collection refused: %v", err)
	}
	_, err := ParseCampaign([]byte("kind: watch\n" + base))
	if err == nil || !strings.Contains(err.Error(), `kind "watch" is not a campaign`) {
		t.Fatalf("kind: watch error = %v", err)
	}
}

func TestParseDocumentRefusesOwnerOnCampaign(t *testing.T) {
	_, err := ParseDocument([]byte("owner: watch/w\n" + string(minimalCampaign(1, "telemetry: true", "wifi"))))
	if err == nil || !strings.Contains(err.Error(), "collection documents cannot have an owner") {
		t.Fatalf("owner error = %v", err)
	}
}

func TestParseDocumentReportsCampaignErrors(t *testing.T) {
	// The collection decoder is ParseCampaign: its validation reaches callers
	// unchanged.
	_, err := ParseDocument([]byte(strings.Replace(string(minimalCampaign(1, "telemetry: true", "wifi")), "upload: {when: wifi}", "upload: {when: sometimes}", 1)))
	if err == nil || !strings.Contains(err.Error(), "upload.when") {
		t.Fatalf("campaign validation error = %v", err)
	}
}

func TestDeployedPlanDoesNotStoreKind(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := m.DeployCampaign([]byte("kind: collection\n" + string(minimalCampaign(1, "telemetry: true", "wifi"))))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(m.campaignDir(), campaign.Name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(stored, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["kind"]; ok {
		t.Fatalf("stored plan carries kind:\n%s", stored)
	}
}

func TestParseCampaignKindDoesNotChangeValue(t *testing.T) {
	plain, err := ParseCampaign(minimalCampaign(1, "telemetry: true", "wifi"))
	if err != nil {
		t.Fatal(err)
	}
	withKind, err := ParseCampaign([]byte("kind: collection\n" + string(minimalCampaign(1, "telemetry: true", "wifi"))))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, withKind) {
		t.Fatalf("campaigns differ:\n%+v\n%+v", plain, withKind)
	}
	out, err := yaml.Marshal(withKind)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "kind") {
		t.Fatalf("marshalled campaign carries kind:\n%s", out)
	}
}
