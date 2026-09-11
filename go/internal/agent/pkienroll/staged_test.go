package pkienroll

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStageWritesTheCredential(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wendy-agent")
	path, err := Stage(dir, StagedEnrollment{
		Token:       "tok-abc",
		TenantUUID:  "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		DeviceID:    "sh/wendy/2/408",
		Environment: "dev",
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if want := filepath.Join(dir, StagedFileName); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	var got map[string]string
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading staged file: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding staged file: %v", err)
	}
	if got["token"] != "tok-abc" || got["deviceID"] != "sh/wendy/2/408" {
		t.Errorf("staged = %v", got)
	}
	// Optional fields that were not given stay absent, so the agent's own
	// defaulting decides them rather than an empty string.
	if _, present := got["csrEndpoint"]; present {
		t.Error("csrEndpoint was written although it was not given")
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat: %v", statErr)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("mode = %v, want 0600: it holds a bearer credential", mode)
		}
	}
}

func TestValidateToken(t *testing.T) {
	for name, tc := range map[string]struct {
		token string
		want  error
	}{
		"a plain token":          {"a1b2c3", nil},
		"a JWT":                  {"eyJhbGciOiJub25lIn0.eyJhIjoxfQ.sig", nil},
		"empty":                  {"", ErrNoToken},
		"only spaces":            {"   ", ErrNoToken},
		"a trailing newline":     {"a1b2c3\n", ErrMalformedToken},
		"an embedded newline":    {"a1b2\nc3", ErrMalformedToken},
		"a carriage return":      {"a1b2c3\r", ErrMalformedToken},
		"a whole metadata block": {"token_id: x\ntoken_value: a1b2c3\n", ErrMalformedToken},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateToken(tc.token)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateToken = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateToken = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStageRefusesAnUnsendableToken(t *testing.T) {
	// Nothing may be written: the agent would redeem it, fail in net/http
	// before reaching pki-core, retry three times and then tell the operator
	// to mint a fresh token that was never needed.
	dir := t.TempDir()
	if _, err := Stage(dir, StagedEnrollment{Token: "tok-abc\n"}); !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("Stage = %v, want ErrMalformedToken", err)
	}
	if _, err := os.Stat(filepath.Join(dir, StagedFileName)); !os.IsNotExist(err) {
		t.Error("a file was staged for an unsendable token")
	}
}
