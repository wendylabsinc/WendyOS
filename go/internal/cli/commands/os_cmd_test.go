package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestValidateOSUpdateIdentityAllowsWendyOSBeforeMenderCheck(t *testing.T) {
	osVersion := "WendyOS-0.10.4"
	resp := &agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: &osVersion}
	if err := validateOSUpdateIdentity(resp); err != nil {
		t.Fatalf("validateOSUpdateIdentity() error = %v, want nil", err)
	}
}

func TestValidateOSUpdateTarget(t *testing.T) {
	strp := func(s string) *string { return &s }

	tests := []struct {
		name string
		resp *agentpb.GetAgentVersionResponse
		want string
	}{
		{
			name: "generic setup is not compatible",
			resp: &agentpb.GetAgentVersionResponse{Os: "darwin"},
			want: osUpdateUnsupportedMessage,
		},
		{
			name: "linux host with agent is not WendyOS",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux"},
			want: linuxOSUpdateUnsupportedMessage,
		},
		{
			name: "linux host with mender is still not WendyOS",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", Featureset: []string{"mender"}},
			want: linuxOSUpdateUnsupportedMessage,
		},
		{
			name: "WendyOS without mender is unsupported",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: strp("WendyOS-0.10.4")},
			want: wendyOSMissingMenderMessage,
		},
		{
			name: "WendyOS version with mender is supported",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", OsVersion: strp("WendyOS-0.10.4"), Featureset: []string{"mender"}},
		},
		{
			name: "WendyOS device type with mender is supported",
			resp: &agentpb.GetAgentVersionResponse{Os: "linux", DeviceType: strp("raspberry-pi-5"), Featureset: []string{"mender"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOSUpdateTarget(tc.resp)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateOSUpdateTarget() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateOSUpdateTarget() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("validateOSUpdateTarget() error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}
