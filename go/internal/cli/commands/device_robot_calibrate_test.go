package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// TestRobotCalibrateCommandTree pins the surface the issue asks for, including
// that `calibrate status` and `calibrate clear` do not collide with
// `calibrate <id>` — they share an argument position, so a procedure called
// "status" would be unreachable (which profile validation refuses).
func TestRobotCalibrateCommandTree(t *testing.T) {
	root := newDeviceRobotCmd()
	calibrate, _, err := root.Find([]string{"calibrate"})
	if err != nil {
		t.Fatalf("finding calibrate: %v", err)
	}
	if calibrate.Name() != "calibrate" {
		t.Fatalf("found %q", calibrate.Name())
	}

	tests := []struct {
		args     []string
		wantName string
	}{
		{args: []string{"calibrate", "status"}, wantName: "status"},
		{args: []string{"calibrate", "clear"}, wantName: "clear"},
		{args: []string{"calibrate", "profiles"}, wantName: "profiles"},
		// An id falls through to calibrate itself rather than being looked up
		// as a subcommand.
		{args: []string{"calibrate", "joint-range"}, wantName: "calibrate"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cmd, _, err := root.Find(tt.args)
			if err != nil {
				t.Fatalf("Find(%v): %v", tt.args, err)
			}
			if cmd.Name() != tt.wantName {
				t.Fatalf("Find(%v) = %q, want %q", tt.args, cmd.Name(), tt.wantName)
			}
		})
	}

	// Every subcommand under `calibrate` shares its argument position with a
	// procedure id, so a profile declaring a calibration by that name would be
	// unreachable. Asserting the two lists agree is what keeps adding a
	// subcommand from silently creating one.
	reserved := map[string]bool{}
	for _, id := range robotcal.ReservedProcedureIDs() {
		reserved[id] = true
	}
	for _, sub := range calibrate.Commands() {
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		if !reserved[sub.Name()] {
			t.Errorf("`calibrate %s` is registered but %q is not a reserved procedure id: "+
				"a profile could declare a calibration by that name and it would be unreachable", sub.Name(), sub.Name())
		}
	}

	for _, flag := range []string{"profile", "unit", "stable-id"} {
		if calibrate.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("--%s is missing", flag)
		}
	}
	// Rule 6: there is no flag that runs a human step without the human.
	for _, forbidden := range []string{"non-interactive", "yes", "force"} {
		if calibrate.Flags().Lookup(forbidden) != nil {
			t.Errorf("--%s must not exist: steps that need a person have no non-interactive mode", forbidden)
		}
	}
}

func TestRobotProfilesListsTheShippedProfiles(t *testing.T) {
	cmd := newRobotProfilesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)

	prev := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = prev })

	if err := cmd.Execute(); err != nil {
		t.Fatalf("profiles: %v", err)
	}
	var entries []struct {
		Kind     string   `json:"kind"`
		Joints   int      `json:"joints"`
		Unit     string   `json:"unit"`
		Requires []string `json:"requires_calibration"`
	}
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("parsing --json output: %v\n%s", err, out.String())
	}
	if len(entries) < 2 {
		t.Fatalf("entries = %d, want the humanoid and the arm", len(entries))
	}
	byKind := map[string]int{}
	for _, e := range entries {
		byKind[e.Kind] = e.Joints
		if e.Unit == "" {
			t.Errorf("%s reports no joint unit", e.Kind)
		}
	}
	if byKind["unitree-g1"] == byKind["so101"] {
		t.Error("the two shipped profiles have the same joint count, which suggests one is not real")
	}
}

// TestCalibrationStoreRefusesRatherThanWritingToTheLaptop: a calibration is a
// fact about the robot, and a per-robot fact stored per-laptop fails silently
// the moment someone else connects.
func TestCalibrationStoreRefusesRatherThanWritingToTheLaptop(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	if _, err := resolveCalibrationStore(); err == nil {
		t.Fatal("off-device calibration must refuse until there is a transport to the device's store")
	} else if !strings.Contains(err.Error(), robotcal.DefaultRoot) {
		t.Fatalf("the refusal must say where the store belongs, got: %v", err)
	}

	t.Setenv("WENDY_AGENT_SOCKET", "/var/lib/wendy/agent-control/agent.sock")
	store, err := resolveCalibrationStore()
	if err != nil {
		t.Fatalf("on-device the store is a file the agent owns: %v", err)
	}
	if store.Describe() != robotcal.DefaultRoot {
		t.Fatalf("store root = %q, want %q", store.Describe(), robotcal.DefaultRoot)
	}
}

// TestJointSourceBackendsRefuseByName checks that a profile selecting a backend
// this build cannot open is told what is missing, rather than silently falling
// back to another source — and that the explanation comes from the profile
// rather than from a case arm named after a vendor.
func TestJointSourceBackendsRefuseByName(t *testing.T) {
	open := openJointSource(nil)

	t.Run("an unknown backend lists what this build can open", func(t *testing.T) {
		_, err := open(t.Context(), robotcal.JointSourceSpec{Backend: "something-invented"})
		if err == nil {
			t.Fatal("want a refusal")
		}
		for _, want := range []string{"something-invented", backendROS2JointStates} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("the profile's own explanation is quoted verbatim", func(t *testing.T) {
		const note = "this bus needs a device-side backend bound by stable id"
		_, err := open(t.Context(), robotcal.JointSourceSpec{Backend: "some-vendor-bus", Note: note})
		if err == nil {
			t.Fatal("want a refusal")
		}
		if !strings.Contains(err.Error(), note) {
			t.Fatalf("error = %v, want it to carry the profile's note", err)
		}
	})

	// The shipped profiles must each either open or refuse with something
	// useful to say; a profile naming an unopenable backend and explaining
	// nothing is the case this guards against.
	for _, kind := range robotcal.ProfileKinds() {
		t.Run(kind, func(t *testing.T) {
			p, err := robotcal.LoadProfile(kind)
			if err != nil {
				t.Fatal(err)
			}
			if p.Joints.Source.Backend == backendROS2JointStates {
				t.Skip("this build can open it")
			}
			if p.Joints.Source.Note == "" {
				t.Fatalf("profile %q selects backend %q, which this build cannot open, and says nothing "+
					"about why — the explanation belongs in the profile, not in a case arm in the CLI",
					kind, p.Joints.Source.Backend)
			}
			_, err = open(t.Context(), p.Joints.Source)
			if err == nil {
				t.Fatalf("expected a refusal for backend %q", p.Joints.Source.Backend)
			}
			if !strings.Contains(err.Error(), p.Joints.Source.Backend) ||
				!strings.Contains(err.Error(), strings.SplitN(p.Joints.Source.Note, " ", 4)[0]) {
				t.Fatalf("refusal = %v, want it to name the backend and quote the profile", err)
			}
		})
	}
}
