package spec

import "testing"

func TestValidateRejectsEntrypointOnNonFinalStage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "deps", From: "debian:12", Entrypoint: &Entrypoint{Exec: []string{"/bin/x"}}},
		{Name: "app", From: "debian:12"},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for entrypoint on a non-final stage, got nil")
	}
}

func TestValidateRejectsUserOnNonFinalStage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "deps", From: "debian:12", User: "root"},
		{Name: "app", From: "debian:12"},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for user on a non-final stage, got nil")
	}
}

func TestValidateRejectsReservedStageName(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{{Name: "local", From: "debian:12"}}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for stage named \"local\", got nil")
	}
}

func TestValidateRejectsDuplicateStageNames(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12"},
		{Name: "app", From: "debian:12"},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for duplicate stage names, got nil")
	}
}

func TestValidateAcceptsEntrypointAndUserOnFinalStage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Entrypoint: &Entrypoint{Exec: []string{"/bin/x"}}, User: "65532"},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateRejectsEmptyInstallBlock(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Install: &Install{}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for an install block with nothing set, got nil")
	}
}

func TestValidateRejectsAptWithNoPackages(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Install: &Install{Apt: &AptInstall{}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for apt with no packages, got nil")
	}
}

func TestValidateRejectsApkWithNoPackages(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "alpine:3.20", Install: &Install{Apk: &ApkInstall{}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for apk with no packages, got nil")
	}
}

func TestValidateAcceptsAptWithPackages(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Install: &Install{Apt: &AptInstall{Packages: []string{"curl"}}}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateRejectsPipWithNothingSet(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for pip with neither requirements nor packages, got nil")
	}
}

func TestValidateRejectsUnknownNpmManager(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "node:20-slim", Install: &Install{Npm: &NpmInstall{Manager: "bower"}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for an unknown npm manager, got nil")
	}
}

func TestValidateRejectsEmptyCopyPaths(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "local", Paths: []string{}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for empty copy.paths, got nil")
	}
}

func TestValidateRejectsRootCopyPath(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "local", Paths: []string{"/"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for copying \"/\", got nil")
	}
}

func TestValidateRejectsMultiplePathsWithoutDest(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "local", Paths: []string{"a", "b"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for multiple copy paths with no dest, got nil")
	}
}

func TestValidateRejectsCopyFromUnknownStage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "ghost", Paths: []string{"a"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for copy.from referencing an unknown stage, got nil")
	}
}

func TestValidateRejectsCopyFromSelf(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "app", Paths: []string{"x"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a stage copying from itself, got nil")
	}
}

func TestValidateAcceptsCopyFromPriorStageAndLocal(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "deps", From: "debian:12"},
		{Name: "app", From: "debian:12", Copy: []CopyEntry{
			{From: "deps", Paths: []string{"/out"}},
			{From: "local", Paths: []string{"app.py"}},
		}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateRejectsUnsupportedBuildLang(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Build: &Build{Lang: "cobol"}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for an unsupported build.lang, got nil")
	}
}

func TestValidateRejectsUnsupportedBuildProfile(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "rust:1", Build: &Build{Lang: "rust", Profile: "fast"}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for an unsupported build.profile, got nil")
	}
}

func TestValidateRejectsLeadingDashInAptPackage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Install: &Install{Apt: &AptInstall{Packages: []string{"--index-url"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for an apt package entry starting with \"-\", got nil")
	}
}

func TestValidateRejectsNewlineInPipPackage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{Packages: []string{"flask\nUSER root\nENV LEAK=1"}}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in a pip package name, got nil")
	}
}

func TestValidateRejectsShellMetacharacterInCopyDest(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "local", Paths: []string{"a.txt"}, Dest: "/app/ && echo pwned"}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a shell metacharacter embedded in copy.dest, got nil")
	}
}

func TestValidateRejectsNewlineInEntrypointArg(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Entrypoint: &Entrypoint{Exec: []string{"python3\nUSER root"}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in an entrypoint arg, got nil")
	}
}

func TestValidateRejectsNewlineInStageName(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app\nFROM evil", From: "debian:12"},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in a stage name, got nil")
	}
}

func TestValidateAcceptsPipVersionSpecifiers(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{Packages: []string{"flask>=2.0,<3.0", "requests[security]"}}}}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("expected no error for legitimate pip version specifiers, got %v", err)
	}
}

func TestValidateAcceptsNormalPackageNames(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Install: &Install{Apt: &AptInstall{Packages: []string{"curl", "git"}}}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("expected no error for normal package names, got %v", err)
	}
}

func TestValidateRejectsNewlineInFrom(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12\nUSER root"},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in from, got nil")
	}
}

func TestValidateRejectsNewlineInUser(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", User: "root\nENV LEAK=1"},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in user, got nil")
	}
}

func TestValidateRejectsLeadingDashInApkPackage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "alpine:3.20", Install: &Install{Apk: &ApkInstall{Packages: []string{"--repository=http://evil.example"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for an apk package entry starting with \"-\", got nil")
	}
}

func TestValidateRejectsNewlineInPipRequirements(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{Requirements: "requirements.txt\nUSER root"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in install.pip.requirements, got nil")
	}
}

func TestValidateRejectsShellMetacharacterInCopyFrom(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "deps", From: "debian:12"},
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "deps; echo pwned", Paths: []string{"a"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a shell metacharacter embedded in copy.from, got nil")
	}
}

func TestValidateRejectsNewlineInCopyPaths(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "local", Paths: []string{"a.txt\nUSER root"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a newline embedded in a copy.paths entry, got nil")
	}
}

func TestValidateRejectsLeadingDashInPipPackage(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{Packages: []string{"--index-url"}}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a pip package entry starting with \"-\", got nil")
	}
}

func TestValidateRejectsWhitespaceInCopyPaths(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{{From: "local", Paths: []string{"a b.txt"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a space embedded in a copy.paths entry, got nil")
	}
}

func TestValidateAcceptsPipEnvironmentMarker(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{
			Packages: []string{"flask>=2.0; python_version>='3.8'"},
		}}}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("expected no error for a PEP 508 environment marker, got %v", err)
	}
}

func TestValidateRejectsLeadingDashInCopyPaths(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{
			{From: "local", Paths: []string{"--from=alpine:3.20", "/etc/alpine-release"}, Dest: "/app/"},
		}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a copy.paths entry starting with \"-\", got nil")
	}
}

func TestValidateRejectsLeadingDashInCopyFrom(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "deps", From: "debian:12"},
		{Name: "app", From: "debian:12", Copy: []CopyEntry{
			{From: "--chown=root", Paths: []string{"a.txt"}},
		}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a copy.from entry starting with \"-\", got nil")
	}
}

func TestValidateRejectsLeadingDashInCopyDest(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{
			{From: "local", Paths: []string{"a.txt"}, Dest: "--chmod=777"},
		}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a copy.dest starting with \"-\", got nil")
	}
}

func TestValidateRejectsLeadingDashInPipRequirements(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{Requirements: "--index-url"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for install.pip.requirements starting with \"-\", got nil")
	}
}

func TestValidateRejectsWhitespaceInPipRequirements(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "python:3.12-slim", Install: &Install{Pip: []PipInstall{{Requirements: "req.txt sub/secret.txt"}}}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a space embedded in install.pip.requirements, got nil")
	}
}

func TestValidateRejectsVerticalTabInCopyPaths(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{
			{From: "local", Paths: []string{"\v--from=alpine:3.20", "/etc/alpine-release"}, Dest: "/app/"},
		}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a vertical tab embedded in a copy.paths entry, got nil")
	}
}

func TestValidateRejectsFormFeedInCopyDest(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{
		{Name: "app", From: "debian:12", Copy: []CopyEntry{
			{From: "local", Paths: []string{"a.txt"}, Dest: "/app/\f--chown=root"},
		}},
	}}
	if err := f.Validate(); err == nil {
		t.Fatal("expected an error for a form feed embedded in copy.dest, got nil")
	}
}

// An entrypoint with no argv compiles to a broken ENTRYPOINT [] — and with
// source: set, to a bash wrapper that execs nothing.
func TestValidateRejectsEmptyEntrypointExec(t *testing.T) {
	for _, e := range []*Entrypoint{{}, {Exec: []string{}}, {Source: "/opt/setup.bash"}} {
		f := &File{Version: 1, Stages: []Stage{{Name: "app", From: "debian:12", Entrypoint: e}}}
		if err := f.Validate(); err == nil {
			t.Errorf("Validate accepted entrypoint %+v with no exec", e)
		}
	}
}

// paths: [""] passes the cardinality check but compiles to a COPY with a
// missing source and, via the dest default, a blank destination.
func TestValidateRejectsEmptyCopyPath(t *testing.T) {
	f := &File{Version: 1, Stages: []Stage{{
		Name: "app", From: "debian:12",
		Copy: []CopyEntry{{From: "local", Paths: []string{""}}},
	}}}
	if err := f.Validate(); err == nil {
		t.Fatal("Validate accepted a copy entry with an empty path")
	}
}
