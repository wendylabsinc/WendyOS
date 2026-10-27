package simenv

import (
	"encoding/xml"
	"strings"
	"testing"
)

// parseXML proves the rendered scene is well-formed by decoding every token.
func parseXML(t *testing.T, b []byte) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("scene.xml is not well-formed XML: %v\n---\n%s", err, b)
		}
	}
}

// jointsAndMotorBindings walks the scene and returns the set of joint names it
// defines and the joint each motor binds to (empty string for a motor that
// names none). MuJoCo refuses to compile a scene whose motor has no
// transmission target or targets a joint the body does not define, so a test
// can assert that structural rule without the mujoco library.
func jointsAndMotorBindings(t *testing.T, b []byte) (map[string]bool, []string) {
	t.Helper()
	joints := map[string]bool{}
	var motorBindings []string
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("decode scene.xml: %v", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		attr := func(name string) string {
			for _, a := range start.Attr {
				if a.Name.Local == name {
					return a.Value
				}
			}
			return ""
		}
		switch start.Name.Local {
		case "joint":
			if n := attr("name"); n != "" {
				joints[n] = true
			}
		case "motor":
			motorBindings = append(motorBindings, attr("joint"))
		}
	}
	return joints, motorBindings
}

func TestRenderMJCFWellFormed(t *testing.T) {
	env := Env{
		SchemaVersion: SchemaVersion,
		Campaign:      "demo",
		Profile:       ProfileCustom,
		Observations: []Observation{
			{Name: "camera_front", Kind: "image", Source: "v4l2:/dev/video0", Dims: []int{720, 1280, 3}, PixelFormat: "yuyv422"},
			{Name: "ros2_imu_data", Kind: "vector", Source: "ros2:cyclone:domain-0:/imu/data", Dims: []int{10}},
			{Name: "ros2_joint_states", Kind: "vector", Source: "ros2:cyclone:domain-0:/joint_states"},
		},
		Action: &Action{
			Stream:   "/cmd_vel",
			Channels: []string{"linear.x", "linear.y", "angular.z"},
			Low:      []float64{-0.8, -0.5, -1},
			High:     []float64{0.8, 0.5, 1},
		},
	}
	out, err := RenderMJCF(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parseXML(t, out)

	s := string(out)
	for _, want := range []string{
		`<camera name="camera_front"`,
		`ctrlrange="-0.8 0.8"`,
		`ctrllimited="true"`,
		`PLACEHOLDER actuated body`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("scene.xml missing %q\n---\n%s", want, s)
		}
	}
	// One motor per action channel.
	if got := strings.Count(s, "<motor "); got != 3 {
		t.Fatalf("motor count = %d, want 3", got)
	}
}

// TestRenderMJCFActuatorsBindDefinedJoints is the structural guard: every motor
// must bind to a joint the scene defines. The first draft emitted motors with
// no joint at all, which parses as XML but fails to compile in MuJoCo with
// "missing transmission target for actuator".
func TestRenderMJCFActuatorsBindDefinedJoints(t *testing.T) {
	cases := []struct {
		name    string
		profile string
	}{
		{"custom", ProfileCustom},
		{"known go2", "go2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{
				SchemaVersion: SchemaVersion,
				Campaign:      "demo",
				Profile:       tc.profile,
				Action: &Action{
					Stream:   "cmd",
					Channels: []string{"wheel_l", "wheel_r"},
					Low:      []float64{-1, -1},
					High:     []float64{1, 1},
				},
			}
			out, err := RenderMJCF(env)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			parseXML(t, out)
			joints, motorBindings := jointsAndMotorBindings(t, out)
			if len(motorBindings) != 2 {
				t.Fatalf("motor count = %d, want 2", len(motorBindings))
			}
			for i, j := range motorBindings {
				if j == "" {
					t.Fatalf("motor %d has no joint target", i)
				}
				if !joints[j] {
					t.Fatalf("motor %d binds joint %q which the scene does not define; defined: %v", i, j, joints)
				}
			}
		})
	}
}

func TestRenderMJCFKnownProfileDiffersFromCustom(t *testing.T) {
	base := Env{SchemaVersion: SchemaVersion, Campaign: "demo"}

	base.Profile = ProfileCustom
	customOut, err := RenderMJCF(base)
	if err != nil {
		t.Fatalf("custom render: %v", err)
	}
	parseXML(t, customOut)

	base.Profile = "go2"
	knownOut, err := RenderMJCF(base)
	if err != nil {
		t.Fatalf("known render: %v", err)
	}
	parseXML(t, knownOut)

	custom, known := string(customOut), string(knownOut)
	// The scaffolds differ by the profile comment, not by an unverified model
	// include: binding the campaign action to a real robot's joints needs a
	// mapping the campaign does not carry, so neither scene includes a model.
	if !strings.Contains(custom, "Custom profile") {
		t.Fatalf("custom scene should name the custom profile:\n%s", custom)
	}
	if !strings.Contains(known, "Intended robot profile go2") || !strings.Contains(known, "go/simulator/go2") {
		t.Fatalf("known scene should name the go2 model to wire in:\n%s", known)
	}
	if strings.Contains(custom, "<include") || strings.Contains(known, "<include") {
		t.Fatalf("no scene should include an unverified model")
	}
	if custom == known {
		t.Fatalf("custom and known scenes should differ")
	}
}

func TestRenderMJCFEmpty(t *testing.T) {
	out, err := RenderMJCF(Env{SchemaVersion: SchemaVersion, Campaign: "bare", Profile: ProfileCustom})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parseXML(t, out)
	s := string(out)
	if strings.Contains(s, "<sensor>") {
		t.Fatalf("empty scene should have no sensor block:\n%s", s)
	}
	if strings.Contains(s, "<actuator>") {
		t.Fatalf("empty scene should have no actuator block:\n%s", s)
	}
}
