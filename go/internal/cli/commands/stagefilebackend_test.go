package commands

import (
	"strings"
	"testing"
)

// TestStagefileBackendLLB covers the selection order (flag beats env, env
// used when no flag, default is dockerfile) and the two hard-error cases: an
// unrecognised value, and an explicit llb request combined with the
// apple-container builder — which has no BuildKit daemon underneath it to
// solve an LLB definition against.
func TestStagefileBackendLLB(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		env        string
		builder    string
		wantLLB    bool
		wantErr    bool
		wantErrSub string
	}{
		{
			name:    "no flag, no env defaults to dockerfile",
			wantLLB: false,
		},
		{
			name:    "env alone selects llb",
			env:     "llb",
			wantLLB: true,
		},
		{
			name:    "env alone selects dockerfile explicitly",
			env:     "dockerfile",
			wantLLB: false,
		},
		{
			name:    "flag beats env when both set",
			flag:    "dockerfile",
			env:     "llb",
			wantLLB: false,
		},
		{
			name:    "flag beats env the other way",
			flag:    "llb",
			env:     "dockerfile",
			wantLLB: true,
		},
		{
			name:       "invalid flag value is an error",
			flag:       "buildx",
			wantErr:    true,
			wantErrSub: "--stagefile-backend",
		},
		{
			name:       "invalid env value is an error",
			env:        "buildx",
			wantErr:    true,
			wantErrSub: "WENDY_STAGEFILE_BACKEND",
		},
		{
			name:       "llb with apple-container builder is an error, not a fallback",
			flag:       "llb",
			builder:    imageBuilderAppleContainer,
			wantErr:    true,
			wantErrSub: imageBuilderAppleContainer,
		},
		{
			name:    "dockerfile with apple-container builder is fine",
			flag:    "dockerfile",
			builder: imageBuilderAppleContainer,
			wantLLB: false,
		},
		{
			name:    "llb with docker builder is fine",
			flag:    "llb",
			builder: imageBuilderDocker,
			wantLLB: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv(stagefileBackendEnvVar, tt.env)
			} else {
				t.Setenv(stagefileBackendEnvVar, "")
			}

			got, err := stagefileBackendLLB(tt.flag, tt.builder)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("stagefileBackendLLB(%q, %q) = nil error, want error", tt.flag, tt.builder)
				}
				if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("error = %q, want it to mention %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("stagefileBackendLLB(%q, %q) unexpected error: %v", tt.flag, tt.builder, err)
			}
			if got != tt.wantLLB {
				t.Fatalf("stagefileBackendLLB(%q, %q) = %v, want %v", tt.flag, tt.builder, got, tt.wantLLB)
			}
		})
	}
}
