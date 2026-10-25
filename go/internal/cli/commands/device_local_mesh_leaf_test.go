package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestLocalMeshLeafFlagsBuildRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    *string // nil: leaf_listen absent, so the agent keeps its saved value
		wantErr string
	}{
		{name: "--leaf enables the default listener", args: []string{"--leaf"}, want: proto.String("0.0.0.0:43024")},
		{name: "--leaf=true", args: []string{"--leaf=true"}, want: proto.String("0.0.0.0:43024")},
		{name: "--leaf=false disables", args: []string{"--leaf=false"}, want: proto.String("")},
		{name: "explicit address", args: []string{"--leaf-listen", "192.168.1.20:43100"}, want: proto.String("192.168.1.20:43100")},
		{name: "IPv6 wildcard", args: []string{"--leaf-listen=[::]:43024"}, want: proto.String("[::]:43024")},
		{name: "omitted leaf options preserve", args: []string{"--ethernet=true"}, want: nil},
		{name: "both options", args: []string{"--leaf", "--leaf-listen", "0.0.0.0:43024"}, wantErr: "either --leaf or --leaf-listen"},
		{name: "empty address", args: []string{"--leaf-listen="}, wantErr: "--leaf=false"},
		{name: "missing port", args: []string{"--leaf-listen", "0.0.0.0"}, wantErr: "host:port"},
		{name: "port zero", args: []string{"--leaf-listen", "0.0.0.0:0"}, wantErr: "port must be 1-65535"},
		{name: "port out of range", args: []string{"--leaf-listen", "0.0.0.0:65536"}, wantErr: "port must be 1-65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Resolve through the root so the test checks the registered
			// command, by a path that also exists once it moves to `wendy net`.
			cmd, _, err := NewRootCmd().Find([]string{"device", "wendy-net", "configure"})
			if err != nil || cmd.Name() != "configure" {
				t.Fatalf("configure command: %v %v", cmd, err)
			}
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			req, err := localMeshRequest(cmd)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (tc.want == nil) != (req.LeafListen == nil) || (tc.want != nil && *req.LeafListen != *tc.want) {
				t.Fatalf("leaf_listen = %v, want %v", req.LeafListen, tc.want)
			}
			// A leaf option is a setting of its own and changes nothing else.
			if tc.want != nil && (req.Participate != nil || req.Roam != nil || req.ShareUplink != nil || req.Nan != nil ||
				req.Ble != nil || req.Ethernet != nil || req.InfrastructureWifi != nil) {
				t.Fatalf("leaf option set other settings: %+v", req)
			}
		})
	}
	// Combined with other options, every given setting is sent.
	cmd, _, err := NewRootCmd().Find([]string{"device", "wendy-net", "configure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"--participate=true", "--infrastructure-wifi=true", "--leaf"}); err != nil {
		t.Fatal(err)
	}
	req, err := localMeshRequest(cmd)
	if err != nil || !req.GetParticipate() || !req.GetInfrastructureWifi() || req.GetLeafListen() != "0.0.0.0:43024" || req.Ethernet != nil {
		t.Fatalf("combined update lost a setting: %+v %v", req, err)
	}
}

func TestLocalMeshStatusShowsLeafListen(t *testing.T) {
	previousJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = previousJSON })
	for _, tc := range []struct {
		leafListen, text, json string
	}{
		{"0.0.0.0:43024", "Lite leaf listen: 0.0.0.0:43024\n", `"leafListen": "0.0.0.0:43024"`},
		{"", "Lite leaf listen: off\n", `"leafListen": ""`},
	} {
		status := &pb.LocalMeshStatus{Configured: &pb.LocalMeshConfiguration{Participate: true, LeafListen: tc.leafListen}}
		for _, asJSON := range []bool{false, true} {
			jsonOutput = asJSON
			cmd := &cobra.Command{}
			var output bytes.Buffer
			cmd.SetOut(&output)
			if err := printLocalMeshStatus(cmd, status); err != nil {
				t.Fatal(err)
			}
			want := tc.text
			if asJSON {
				want = tc.json
			}
			if !strings.Contains(output.String(), want) {
				t.Fatalf("json=%t output lacks %q:\n%s", asJSON, want, output.String())
			}
		}
	}
}
