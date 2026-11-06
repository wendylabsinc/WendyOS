package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedPythonRuntimeRequirement(t *testing.T) {
	for _, tc := range []struct{ requirement, want string }{
		{"", "3.11"}, {">=3.12", "3.12"}, {">=3.12,<3.13", "3.12"},
		{">=3.10", "3.11"}, {">=3.12,!=3.12.*", "3.13"}, {"~=3.12", "3.12"},
		{"==3.13.*", "3.13"}, {"==3.*", "3.11"}, {"<3.11", "3.10"},
		{">=3.16", ""}, {"==3.12.2", ""}, {">=3.12.2", ""}, {"==3.12", ""},
		{">=3.12rc1", ""}, {">=3.13,<3.12", ""}, {">=3.12\nRUN bad", ""},
	} {
		t.Run(tc.requirement, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nrequires-python = '"+tc.requirement+"'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := generatePythonDockerfile(dir, false)
			if tc.want == "" {
				if err == nil {
					t.Fatal("incompatible/unsupported requirement accepted")
				}
				if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); !os.IsNotExist(err) {
					t.Fatalf("generated an unsafe Dockerfile: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(b), "FROM python:"+tc.want+"-slim\n") {
				t.Fatalf("wrong runtime: %s", b)
			}
		})
	}
}

func TestPythonRuntimeInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := generatePythonDockerfile(dir, false); err == nil {
		t.Fatal("invalid TOML accepted")
	}
}
