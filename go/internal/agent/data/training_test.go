package data

import (
	"reflect"
	"strings"
	"testing"
)

// validTrainingBlock is the exact training block from the schema design, ready
// to splice into the people example ahead of its top-level capture block. It
// keeps the leading and trailing newlines so it slots in where the top-level
// "capture:" key begins, not the indented capture under a source.
const validTrainingBlock = `
training:
  rate_hz: 20
  action:
    stream: cmd
    channels: [wheel_l, wheel_r]
    range: [[-1, 1], [-1, 1]]
    unit: m/s

capture:
`

// trainingCampaign returns the people example with a valid training block added.
func trainingCampaign(t *testing.T) string {
	t.Helper()
	raw := string(peopleCampaign(t))
	withBlock := strings.Replace(raw, "\ncapture:\n", validTrainingBlock, 1)
	if withBlock == raw {
		t.Fatal("could not insert training block into the example campaign")
	}
	return withBlock
}

func TestTrainingBlockParsesTypedFields(t *testing.T) {
	campaign, err := ParseCampaign([]byte(trainingCampaign(t)))
	if err != nil {
		t.Fatal(err)
	}
	tr := campaign.Training
	if tr == nil {
		t.Fatal("training block did not parse")
	}
	if tr.RateHz != 20 {
		t.Fatalf("rate_hz = %v, want 20", tr.RateHz)
	}
	if tr.Action.Stream != "cmd" {
		t.Fatalf("stream = %q, want cmd", tr.Action.Stream)
	}
	if !reflect.DeepEqual(tr.Action.Channels, []string{"wheel_l", "wheel_r"}) {
		t.Fatalf("channels = %v", tr.Action.Channels)
	}
	if !reflect.DeepEqual(tr.Action.Range, [][]float64{{-1, 1}, {-1, 1}}) {
		t.Fatalf("range = %v", tr.Action.Range)
	}
	if !reflect.DeepEqual(tr.Action.Unit, TrainingUnit{"m/s"}) {
		t.Fatalf("unit = %v", tr.Action.Unit)
	}
}

func TestTrainingUnitAcceptsListPerChannel(t *testing.T) {
	raw := strings.Replace(trainingCampaign(t), "unit: m/s", "unit: [m/s, rad/s]", 1)
	campaign, err := ParseCampaign([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(campaign.Training.Action.Unit, TrainingUnit{"m/s", "rad/s"}) {
		t.Fatalf("unit = %v, want one per channel", campaign.Training.Action.Unit)
	}
}

func TestTrainingRejectsInvalidConfiguration(t *testing.T) {
	raw := trainingCampaign(t)
	changes := [][2]string{
		{"rate_hz: 20", "rate_hz: 0"},
		{"rate_hz: 20", "rate_hz: 1001"},
		{"channels: [wheel_l, wheel_r]", "channels: []"},
		{"channels: [wheel_l, wheel_r]", "channels: [wheel_l, wheel_l]"},
		{"range: [[-1, 1], [-1, 1]]", "range: [[-1, 1]]"},
		{"range: [[-1, 1], [-1, 1]]", "range: [[1, 1], [-1, 1]]"},
		{"stream: cmd", "stream: Cmd"},
		{"  action:", "  bogus: 1\n  action:"},
		{"unit: m/s", "unit: [m/s, rad/s, deg/s]"},
	}
	for _, change := range changes {
		t.Run(change[1], func(t *testing.T) {
			mutated := strings.Replace(raw, change[0], change[1], 1)
			if mutated == raw {
				t.Fatalf("change %q did not apply", change[1])
			}
			if _, err := ParseCampaign([]byte(mutated)); err == nil {
				t.Fatalf("accepted %s", change[1])
			}
		})
	}
}

func TestTrainingBlockChangesRevision(t *testing.T) {
	base, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	withTraining, err := ParseCampaign([]byte(trainingCampaign(t)))
	if err != nil {
		t.Fatal(err)
	}
	if base.Revision == withTraining.Revision {
		t.Fatal("adding the training block did not change the revision")
	}

	raw := trainingCampaign(t)
	for _, change := range [][2]string{
		{"rate_hz: 20", "rate_hz: 21"},
		{"stream: cmd", "stream: cmdx"},
		{"channels: [wheel_l, wheel_r]", "channels: [wheel_l, wheel_x]"},
		{"range: [[-1, 1], [-1, 1]]", "range: [[-2, 2], [-1, 1]]"},
		{"unit: m/s", "unit: rad/s"},
	} {
		t.Run(change[1], func(t *testing.T) {
			changed, err := ParseCampaign([]byte(strings.Replace(raw, change[0], change[1], 1)))
			if err != nil {
				t.Fatal(err)
			}
			if changed.Revision == withTraining.Revision {
				t.Fatalf("training change %q did not change the revision", change[1])
			}
		})
	}
}

func TestCampaignWithoutTrainingIsDeterministic(t *testing.T) {
	first, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	if first.Training != nil {
		t.Fatal("example campaign unexpectedly carries a training block")
	}
	if first.Revision == "" || first.Revision != second.Revision {
		t.Fatalf("campaign without training is not deterministic: %q vs %q", first.Revision, second.Revision)
	}
}
