package mcp

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestInstallationJobsUseHostBackendWithoutConnection(t *testing.T) {
	s := New(&config.Config{}, nil)
	var resumed onboarding.ResumeOptions
	s.SetInstallationBackend(onboarding.Backend{
		Start: func(_ context.Context, o onboarding.StartOptions) (*onboarding.Job, error) {
			if o.DeviceType != "raspberry-pi-5" || o.Drive != "/dev/test" {
				t.Fatalf("lost install target: %+v", o)
			}
			return &onboarding.Job{}, nil
		},
		Status: func(_ context.Context, id string) (*onboarding.Job, error) {
			if id != "saved-job" {
				t.Fatalf("wrong job: %s", id)
			}
			return &onboarding.Job{}, nil
		},
		Resume: func(_ context.Context, o onboarding.ResumeOptions) (*onboarding.Job, error) {
			resumed = o
			return &onboarding.Job{}, nil
		},
	})
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"os_install_start", map[string]any{"device_type": "raspberry-pi-5", "drive": "/dev/test"}},
		{"os_install_status", map[string]any{"job_id": "saved-job"}},
		{"os_install_resume", map[string]any{"job_id": "saved-job", "target_id": "fingerprint", "confirm_erase": true, "confirm_internal": true}},
	} {
		result, err := srv.GetTool(tc.name).Handler(context.Background(), callToolReq(tc.name, tc.args))
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %v", tc.name, result, err)
		}
	}
	if !resumed.ConfirmErase || !resumed.ConfirmInternal || resumed.TargetID != "fingerprint" || resumed.JobID != "saved-job" {
		t.Fatalf("lost authorization: %+v", resumed)
	}
	if s.GetConn() != nil {
		t.Fatal("installation job changed active connection")
	}
}

func TestInstallResumeRejectsMalformedAuthorizationBeforeBackend(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetInstallationBackend(onboarding.Backend{Resume: func(context.Context, onboarding.ResumeOptions) (*onboarding.Job, error) {
		t.Fatal("invalid authorization reached backend")
		return nil, nil
	}})
	for _, args := range []map[string]any{
		{}, {"job_id": " "},
		{"job_id": "job", "confirm_erase": true},
		{"job_id": "job", "confirm_erase": "true", "target_id": "fingerprint"},
		{"job_id": "job", "confirm_internal": 1},
	} {
		result, err := s.handleInstallResume(context.Background(), callToolReq("os_install_resume", args))
		if err != nil || !result.IsError {
			t.Fatalf("accepted %+v: %v %v", args, result, err)
		}
	}
}

func TestInstallationJobAnnotationsReflectWrites(t *testing.T) {
	srv, err := New(&config.Config{}, nil).newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		read, destructive bool
	}{
		{"os_install_start", false, false},
		{"os_install_status", true, false},
		{"os_install_resume", false, true},
	} {
		a := srv.GetTool(tc.name).Tool.Annotations
		if a.ReadOnlyHint == nil || *a.ReadOnlyHint != tc.read || a.DestructiveHint == nil || *a.DestructiveHint != tc.destructive {
			t.Fatalf("%s annotations: %+v", tc.name, a)
		}
	}
}
