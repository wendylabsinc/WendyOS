package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCloudEnrollmentUsesLocalTarget(t *testing.T) {
	for _, cmd := range []*cobra.Command{newDeviceEnrollCmd(), newCloudEnrollDeviceCmd()} {
		t.Run(cmd.Name(), func(t *testing.T) {
			cmd.SetContext(context.Background())
			cmd.RunE = func(cmd *cobra.Command, _ []string) error {
				if _, ok := cloudDeviceConfigFromContext(cmd.Context()); ok {
					t.Fatal("enrollment was routed through cloud agent")
				}
				return nil
			}
			wrapCloudDeviceCommands(cmd, func(*cobra.Command) cloudDeviceConfig { return cloudDeviceConfig{DeviceName: "wendy-lite:/dev/test"} })
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	cmd := &cobra.Command{Use: "logs"}
	cmd.SetContext(context.Background())
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if _, ok := cloudDeviceConfigFromContext(cmd.Context()); !ok {
			t.Fatal("ordinary cloud command lost its route")
		}
		return nil
	}
	wrapCloudDeviceCommands(cmd, func(*cobra.Command) cloudDeviceConfig { return cloudDeviceConfig{} })
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
}

// TestEnrollmentCommandVisibility pins where enrollment appears in help.
// `wendy device enroll` is listed directly above `wendy device unenroll`, and
// `wendy cloud enroll-device` stays listed as an alias. The cloud mirror's
// `wendy cloud device enroll` stays registered but hidden: enrolling needs a
// direct LAN/USB path to a device that is not in Cloud yet, so a cloud-tunnel
// twin would advertise a route that cannot work.
func TestEnrollmentCommandVisibility(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_ANALYTICS", "false")

	root := NewRootCmd()
	for _, tc := range []struct {
		path   string
		hidden bool
	}{
		{path: "device enroll", hidden: false},
		{path: "device unenroll", hidden: false},
		{path: "cloud enroll-device", hidden: false},
		{path: "cloud device enroll", hidden: true},
		{path: "cloud device unenroll", hidden: false},
	} {
		cmd, _, err := root.Find(strings.Fields(tc.path))
		if err != nil || cmd.CommandPath() != root.Name()+" "+tc.path {
			t.Fatalf("%s is not registered: cmd=%v err=%v", tc.path, cmd, err)
		}
		if cmd.Hidden != tc.hidden {
			t.Errorf("%s: Hidden = %v, want %v", tc.path, cmd.Hidden, tc.hidden)
		}
	}

	device := helpLines(t, "device")
	enroll, unenroll := helpEntry(device, "enroll"), helpEntry(device, "unenroll")
	if enroll < 0 || unenroll != enroll+1 {
		t.Errorf("wendy device --help should list enroll directly above unenroll (enroll line %d, unenroll line %d):\n%s", enroll, unenroll, strings.Join(device, "\n"))
	}

	if cloud := helpLines(t, "cloud"); helpEntry(cloud, "enroll-device") < 0 {
		t.Errorf("wendy cloud --help should list enroll-device:\n%s", strings.Join(cloud, "\n"))
	}

	cloudDevice := helpLines(t, "cloud", "device")
	if helpEntry(cloudDevice, "enroll") >= 0 {
		t.Errorf("wendy cloud device --help should not list enroll:\n%s", strings.Join(cloudDevice, "\n"))
	}
	if helpEntry(cloudDevice, "unenroll") < 0 {
		t.Errorf("wendy cloud device --help should still list unenroll:\n%s", strings.Join(cloudDevice, "\n"))
	}
}

// helpLines renders `wendy <args> --help` from a fresh command tree.
func helpLines(t *testing.T, args ...string) []string {
	t.Helper()
	root := NewRootCmd()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(new(bytes.Buffer))
	root.SetArgs(append(args, "--help"))
	if err := root.Execute(); err != nil {
		t.Fatalf("wendy %s --help: %v", strings.Join(args, " "), err)
	}
	return strings.Split(out.String(), "\n")
}

// helpEntry returns the index of the help line listing subcommand name, or -1.
func helpEntry(lines []string, name string) int {
	for i, line := range lines {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name && strings.HasPrefix(line, "  ") {
			return i
		}
	}
	return -1
}
