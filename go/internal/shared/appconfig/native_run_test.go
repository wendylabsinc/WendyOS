package appconfig

import "testing"

func TestNativeRunValidation(t *testing.T) {
	for _, tc := range []struct {
		command, cwd, platform string
		valid                  bool
	}{
		{"./serve.py", "", "darwin", true}, {"/opt/homebrew/bin/python3", "data", "darwin/arm64", true},
		{"serve.py", ".", "", true}, {"../serve.py", "", "darwin", false},
		{"serve.py", "../other", "darwin", false}, {"serve.py", "/tmp", "darwin", false},
		{"serve.py", "", "linux/arm64", false}, {".", "", "darwin", false},
	} {
		cfg := &AppConfig{AppID: "sh.wendy.test", Platform: tc.platform, Run: &RunConfig{Command: tc.command, Cwd: tc.cwd}}
		if err := cfg.Validate(); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}
