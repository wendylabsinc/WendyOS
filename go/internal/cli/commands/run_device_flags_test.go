package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func TestRunOptionalDeviceFlags(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		args                     []string
		hil, host                string
		hilSet, hostSet, wantErr bool
	}{
		{name: "omitted"},
		{name: "stray positional", args: []string{"unexpected"}, wantErr: true},
		{name: "app argument after dash", args: []string{"--", "app-arg"}},
		{name: "hil picker", args: []string{"--hil"}, hilSet: true},
		{name: "hil named", args: []string{"--hil=jetson"}, hil: "jetson", hilSet: true},
		{name: "hil spaced", args: []string{"--hil", "jetson"}, hil: "jetson", hilSet: true},
		{name: "build picker", args: []string{"--build-host"}, hostSet: true},
		{name: "build named", args: []string{"--build-host=spark"}, host: "spark", hostSet: true},
		{name: "build spaced", args: []string{"--build-host", "spark"}, host: "spark", hostSet: true},
		{name: "both pickers", args: []string{"--hil", "--build-host"}, hilSet: true, hostSet: true},
		{name: "both named", args: []string{"--hil=jetson", "--build-host=spark"}, hil: "jetson", host: "spark", hilSet: true, hostSet: true},
		{name: "picker and named", args: []string{"--hil", "--build-host=spark"}, host: "spark", hilSet: true, hostSet: true},
		{name: "next flag", args: []string{"--hil", "--yes"}, hilSet: true},
		{name: "ambiguous", args: []string{"--hil", "--build-host", "spark"}, wantErr: true},
		{name: "extra argument", args: []string{"--hil=jetson", "other"}, wantErr: true},
		{name: "after dash", args: []string{"--hil", "--", "jetson"}, wantErr: true},
		{name: "removed flag", args: []string{"--hil-device", "jetson"}, wantErr: true},
		{name: "cloud selector", args: []string{"--hil=cloud://example.com/org/1/asset/42"}, hil: "cloud://example.com/org/1/asset/42", hilSet: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRunCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tt.args)
			called := false
			cmd.RunE = func(cmd *cobra.Command, _ []string) error {
				called = true
				hil, _ := cmd.Flags().GetString("hil")
				host, _ := cmd.Flags().GetString("build-host")
				if strings.TrimSpace(hil) != tt.hil || strings.TrimSpace(host) != tt.host || cmd.Flags().Changed("hil") != tt.hilSet || cmd.Flags().Changed("build-host") != tt.hostSet {
					t.Fatalf("hil=%q host=%q; changed=%v/%v", hil, host, cmd.Flags().Changed("hil"), cmd.Flags().Changed("build-host"))
				}
				return nil
			}
			if err := cmd.Execute(); (err != nil) != tt.wantErr {
				t.Fatalf("error=%v, want error=%v", err, tt.wantErr)
			}
			if called == tt.wantErr {
				t.Fatalf("handler called=%v", called)
			}
		})
	}
}

func TestRunBuildHostPickerGuardsAndCancellation(t *testing.T) {
	oldInteractive, oldPicker, oldDevice := isInteractiveTerminalFn, pickRunBuildHostDevice, deviceFlag
	t.Cleanup(func() {
		isInteractiveTerminalFn, pickRunBuildHostDevice, deviceFlag = oldInteractive, oldPicker, oldDevice
	})
	deviceFlag = "vm:simulator"
	calls := 0
	pickRunBuildHostDevice = func(ctx context.Context) (*SelectedDevice, error) {
		if devicePickerPurposeFromContext(ctx) != buildHostPicker {
			t.Fatal("build host picker lost its purpose")
		}
		calls++
		return nil, ErrUserCancelled
	}
	for _, tt := range []struct {
		args        []string
		interactive bool
		want        string
	}{
		{[]string{"--build-host"}, false, "--build-host=DEVICE"},
		{[]string{"--build-host", "--yes"}, true, "--build-host=DEVICE"},
		{[]string{"--build-host", "--builder=docker"}, true, "cannot be combined"},
	} {
		isInteractiveTerminalFn = func() bool { return tt.interactive }
		cmd := newRunCmd()
		cmd.SetArgs(tt.args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("args=%v: %v", tt.args, err)
		}
	}
	if calls != 0 {
		t.Fatal("guard opened picker")
	}
	isInteractiveTerminalFn = func() bool { return true }
	cmd := newRunCmd()
	cmd.SetArgs([]string{"--build-host"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 || deviceFlag != "vm:simulator" {
		t.Fatalf("calls=%d target=%q", calls, deviceFlag)
	}
}

func TestSelectedBuildHostRetainsReconnectIdentity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		selected SelectedDevice
		want     string
	}{
		{"cloud", SelectedDevice{Agent: &grpcclient.AgentConnection{Addr: "127.0.0.1:1234"}, DefaultSelector: "cloud://example.com/org/1/asset/42"}, "cloud://example.com/org/1/asset/42"},
		{"LAN hostname", SelectedDevice{Agent: &grpcclient.AgentConnection{Addr: "192.0.2.1:50052"}, PinKey: "spark.local"}, "spark.local:50052"},
		{"direct address", SelectedDevice{Agent: &grpcclient.AgentConnection{Addr: "192.0.2.1:50051"}}, "192.0.2.1:50051"},
		{"simulator", SelectedDevice{Agent: &grpcclient.AgentConnection{SimulatorName: "sim"}}, ""},
		{"no agent", SelectedDevice{}, ""},
		{"no address", SelectedDevice{Agent: &grpcclient.AgentConnection{}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectedBuildHostName(&tt.selected)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

type buildPickerCloser struct{ closed bool }

func (c *buildPickerCloser) Close() error { c.closed = true; return nil }

func TestRunBuildHostPickerSelectionAndExplicitBypass(t *testing.T) {
	oldInteractive, oldPicker, oldDevice := isInteractiveTerminalFn, pickRunBuildHostDevice, deviceFlag
	t.Cleanup(func() {
		isInteractiveTerminalFn, pickRunBuildHostDevice, deviceFlag = oldInteractive, oldPicker, oldDevice
	})
	isInteractiveTerminalFn = func() bool { return true }
	deviceFlag = "vm:simulator"
	calls := 0
	closer := &buildPickerCloser{}
	pickRunBuildHostDevice = func(ctx context.Context) (*SelectedDevice, error) {
		if devicePickerPurposeFromContext(ctx) != buildHostPicker {
			t.Fatal("build host picker lost its purpose")
		}
		calls++
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{Addr: "127.0.0.1:1234", ExtraClosers: []io.Closer{closer}}, DefaultSelector: "cloud://example.com/org/1/asset/42"}, nil
	}
	for _, tt := range []struct {
		args      []string
		wantCalls int
	}{
		{[]string{"--hil=jetson", "--detach", "--build-host=spark", "--yes"}, 0},
		{[]string{"--hil=jetson", "--detach"}, 0},
		{[]string{"--hil=jetson", "--detach", "--build-host"}, 1},
	} {
		cmd := newRunCmd()
		cmd.SetArgs(tt.args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		// Stop at the HIL lifecycle check, before contacting either device.
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "attached") {
			t.Fatalf("args=%v: %v", tt.args, err)
		}
		if calls != tt.wantCalls || deviceFlag != "vm:simulator" {
			t.Fatalf("calls=%d target=%q", calls, deviceFlag)
		}
	}
	if !closer.closed {
		t.Fatal("picker connection was not closed")
	}
}

func TestRunOptionalDeviceHelp(t *testing.T) {
	cmd := newRunCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	help := output.String()
	for _, syntax := range []string{"--hil [=DEVICE]", "--build-host [=DEVICE]"} {
		if !strings.Contains(help, syntax) {
			t.Fatalf("missing %q in help: %s", syntax, help)
		}
	}
	if strings.Contains(help, "hil-device") || strings.Contains(help, `string[=" "]`) {
		t.Fatalf("unexpected flag syntax: %s", help)
	}
}
