package spec

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
)

// Validate checks structural rules that don't require any external state
// (no registry lookups, no filesystem access).
func (f *File) Validate() error {
	if f.Version != 1 {
		return fmt.Errorf("unsupported version %d (want 1)", f.Version)
	}
	if len(f.Stages) == 0 {
		return fmt.Errorf("stages: must declare at least one stage")
	}

	priorNames := map[string]bool{}
	lastIdx := len(f.Stages) - 1
	for i, s := range f.Stages {
		if s.Name == "" {
			return fmt.Errorf("stages[%d]: name is required", i)
		}
		if s.Name == "local" {
			return fmt.Errorf("stages[%d]: %q is a reserved name (means the local source tree in copy.from)", i, s.Name)
		}
		if priorNames[s.Name] {
			return fmt.Errorf("stages[%d]: duplicate stage name %q", i, s.Name)
		}
		if s.From == "" {
			return fmt.Errorf("stage %q: from is required", s.Name)
		}
		if s.Platform != "" && s.Platform != "build" {
			return fmt.Errorf("stage %q: platform %q is not supported (only \"build\", meaning $BUILDPLATFORM)", s.Name, s.Platform)
		}
		if err := validateWorkdir(s.Workdir); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateKVMap(s.Args, "args"); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateKVMap(s.Env, "env"); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateNoInjection(&s); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateInstall(s.Install); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateCUDA(&s); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateDownloads(s.Download); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateCopy(s.Copy, priorNames); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateBuild(s.Build); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateBuildWorkdir(&s); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}
		if err := validateHealthcheck(s.Healthcheck); err != nil {
			return fmt.Errorf("stage %q: %w", s.Name, err)
		}

		priorNames[s.Name] = true

		isFinal := i == lastIdx
		finalName := f.Stages[lastIdx].Name
		if !isFinal && s.Entrypoint != nil {
			return fmt.Errorf("stage %q: entrypoint is only allowed on the final stage (%q)", s.Name, finalName)
		}
		// An entrypoint with no argv is not a container that starts and does
		// nothing — it is a broken ENTRYPOINT [] line, and with source: set
		// it is a bash wrapper that execs nothing. healthcheck.exec already
		// carries the same guard for the same reason.
		if s.Entrypoint != nil && len(s.Entrypoint.Exec) == 0 {
			return fmt.Errorf("stage %q: entrypoint.exec must be non-empty", s.Name)
		}
		if !isFinal && s.User != "" {
			return fmt.Errorf("stage %q: user is only allowed on the final stage (%q)", s.Name, finalName)
		}
		if !isFinal && len(s.Cmd) > 0 {
			return fmt.Errorf("stage %q: cmd is only allowed on the final stage (%q)", s.Name, finalName)
		}
		if !isFinal && s.Healthcheck != nil {
			return fmt.Errorf("stage %q: healthcheck is only allowed on the final stage (%q)", s.Name, finalName)
		}
	}
	return nil
}

// validateCUDA ties the stage-level cuda: declaration to the pip groups that
// depend on it, and keeps the compiler's own settings from being contradicted
// by hand.
//
// The errors here are the whole point of the feature: each one names the fix
// rather than describing a schema violation, because the reader is someone
// who does not already know that a GPU wheel needs a different index from the
// runtime that goes with it.
func validateCUDA(s *Stage) error {
	for i, p := range pipGroups(s) {
		if !p.CUDA {
			continue
		}
		field := fmt.Sprintf("install.pip[%d]", i)
		if !s.CUDA {
			return fmt.Errorf("%s sets cuda: but the stage does not — add `cuda: true` to the stage so the CUDA runtime is installed and put on the loader path", field)
		}
		if p.Index != "" || len(p.ExtraIndex) > 0 {
			return fmt.Errorf("%s sets both cuda: and index/extraIndex — cuda: resolves the index from the GPU architecture being built for; drop the explicit one, or drop cuda: to pin the index by hand", field)
		}
	}
	if !s.CUDA {
		return nil
	}
	// The collected directory has to win against the loader paths a Jetson
	// injects at run time, which means the compiler owns LD_LIBRARY_PATH's
	// leading entry. A Stagefile may still extend it — codegen prepends
	// rather than replaces — but a value that starts with something else
	// would be silently rewritten, so say so instead.
	if _, ok := s.Env[LDLibraryPath]; ok {
		return fmt.Errorf("stage sets env.%s and cuda: — a CUDA stage's collected libraries must come first on that path, so the compiler prepends its own directory; remove the env entry, or drop cuda: and set up the loader path by hand", LDLibraryPath)
	}
	return nil
}

// pipGroups is nil-safe access to a stage's pip install groups, so validation
// can iterate them without repeating the two nil checks.
func pipGroups(s *Stage) []PipInstall {
	if s.Install == nil {
		return nil
	}
	return s.Install.Pip
}

// validateAbsPath is the shared shape check for a path the compiler will
// interpolate into a command: present, absolute, and free of the characters
// that make a path ambiguous in that position.
func validateAbsPath(p, fieldDesc string) error {
	if p == "" {
		return fmt.Errorf("%s is required", fieldDesc)
	}
	if err := rejectNewline(p, fieldDesc); err != nil {
		return err
	}
	if err := rejectWhitespace(p, fieldDesc); err != nil {
		return err
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%s must be an absolute path (got %q)", fieldDesc, p)
	}
	return nil
}

func validateWorkdir(w string) error {
	if w == "" {
		return nil
	}
	if err := rejectNewline(w, "workdir"); err != nil {
		return err
	}
	if err := rejectWhitespace(w, "workdir"); err != nil {
		return err
	}
	if !strings.HasPrefix(w, "/") {
		return fmt.Errorf("workdir must be an absolute path (got %q)", w)
	}
	return nil
}

func validateKVMap(m map[string]string, fieldDesc string) error {
	for k, v := range m {
		if k == "" {
			return fmt.Errorf("%s: key must be non-empty", fieldDesc)
		}
		if err := rejectNewline(k, fieldDesc+" key"); err != nil {
			return err
		}
		if err := rejectWhitespace(k, fieldDesc+" key"); err != nil {
			return err
		}
		if err := rejectLeadingDash(k, fieldDesc+" key"); err != nil {
			return err
		}
		if strings.Contains(k, "=") {
			return fmt.Errorf("%s key must not contain \"=\" (got %q)", fieldDesc, k)
		}
		if err := rejectNewline(v, fieldDesc+" value"); err != nil {
			return err
		}
	}
	return nil
}

// validateRepoToken checks a value destined for an apt sources.list line or
// apk repositories file: line-based formats where whitespace separates
// fields, so tokens must be single words.
func validateRepoToken(value, fieldDesc string) error {
	if value == "" {
		return fmt.Errorf("%s must be non-empty", fieldDesc)
	}
	if err := rejectNewline(value, fieldDesc); err != nil {
		return err
	}
	if err := rejectWhitespace(value, fieldDesc); err != nil {
		return err
	}
	return rejectLeadingDash(value, fieldDesc)
}

func validateRepoURL(value, fieldDesc string) error {
	if err := validateRepoToken(value, fieldDesc); err != nil {
		return err
	}
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return fmt.Errorf("%s must be an http(s) URL (got %q)", fieldDesc, value)
	}
	return nil
}

// isFilenameSafe reports whether s is safe to embed in a generated
// filename under /etc/apt: alphanumerics, dash, underscore, dot.
func isFilenameSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// validateSHA256 checks a content digest, with or without the "sha256:"
// prefix. Both the apt signing key and the download step pin content this
// way, and they share this check so the two can't drift on what counts as a
// digest — a difference that would show up as one of them accepting a
// truncated hash.
func validateSHA256(value, fieldDesc string) error {
	sha := strings.TrimPrefix(value, "sha256:")
	if len(sha) != 64 || !isHex(sha) {
		return fmt.Errorf("%s must be a 64-hex-digit sha256 (got %q) — content is pinned, like everything else", fieldDesc, value)
	}
	return nil
}

func validateAptRepositories(repos []AptRepository) error {
	seen := map[string]bool{}
	for _, r := range repos {
		if !isFilenameSafe(r.Name) {
			return fmt.Errorf("install.apt.repositories name %q must be a filename-safe identifier (alphanumerics, dash, underscore, dot)", r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("install.apt.repositories: duplicate name %q", r.Name)
		}
		seen[r.Name] = true
		if err := validateRepoURL(r.URL, "install.apt.repositories url"); err != nil {
			return err
		}
		if len(r.Suites) == 0 {
			return fmt.Errorf("install.apt.repositories %q: suites must be non-empty", r.Name)
		}
		for _, s := range r.Suites {
			if err := validateRepoToken(s, "install.apt.repositories suite"); err != nil {
				return err
			}
		}
		if len(r.Components) == 0 {
			return fmt.Errorf("install.apt.repositories %q: components must be non-empty", r.Name)
		}
		for _, c := range r.Components {
			if err := validateRepoToken(c, "install.apt.repositories component"); err != nil {
				return err
			}
		}
		if err := validateRepoURL(r.Key.URL, "install.apt.repositories key.url"); err != nil {
			return err
		}
		if err := validateSHA256(r.Key.SHA256, fmt.Sprintf("install.apt.repositories %q: key.sha256", r.Name)); err != nil {
			return err
		}
		switch r.Key.Format {
		case "", "binary", "armored":
		default:
			return fmt.Errorf("install.apt.repositories %q: key.format %q is not one of binary, armored", r.Name, r.Key.Format)
		}
	}
	return nil
}

func validateInstall(inst *Install) error {
	if inst == nil {
		return nil
	}
	if inst.Apt == nil && inst.Apk == nil && len(inst.CMake) == 0 && len(inst.Pip) == 0 && inst.Npm == nil && inst.Uv == nil {
		return fmt.Errorf("install: at least one of apt, apk, cmake, pip, npm, uv must be set")
	}
	if inst.Apt != nil {
		if len(inst.Apt.Packages) == 0 {
			return fmt.Errorf("install.apt: packages must be non-empty")
		}
		if err := validateAptRepositories(inst.Apt.Repositories); err != nil {
			return err
		}
	}
	if inst.Apk != nil {
		if len(inst.Apk.Packages) == 0 {
			return fmt.Errorf("install.apk: packages must be non-empty")
		}
		for _, r := range inst.Apk.Repositories {
			if err := validateRepoURL(r, "install.apk.repositories entry"); err != nil {
				return err
			}
		}
	}
	for i, c := range inst.CMake {
		field := fmt.Sprintf("install.cmake[%d]", i)
		if err := validateRepoURL(c.Repository, field+".repository"); err != nil {
			return err
		}
		if len(c.Commit) != 40 || !isHex(c.Commit) {
			return fmt.Errorf("%s.commit must be a full 40-hex Git commit (got %q) — branches and tags can move", field, c.Commit)
		}
		if c.Prefix != "" {
			if err := rejectNewline(c.Prefix, field+".prefix"); err != nil {
				return err
			}
			if !strings.HasPrefix(c.Prefix, "/") {
				return fmt.Errorf("%s.prefix must be an absolute path (got %q)", field, c.Prefix)
			}
		}
		switch c.BuildType {
		case "", "Release", "Debug", "RelWithDebInfo", "MinSizeRel":
		default:
			return fmt.Errorf("%s.buildType %q is not one of Release, Debug, RelWithDebInfo, MinSizeRel", field, c.BuildType)
		}
		if c.Jobs < 0 {
			return fmt.Errorf("%s.jobs must be non-negative (got %d)", field, c.Jobs)
		}
		for k, v := range c.Defines {
			if !isCMakeIdentifier(k) {
				return fmt.Errorf("%s.defines key %q must be a CMake identifier (letters, digits, and underscore; not starting with a digit)", field, k)
			}
			if err := rejectNewline(v, field+".defines value"); err != nil {
				return err
			}
		}
	}
	for i, p := range inst.Pip {
		field := fmt.Sprintf("install.pip[%d]", i)
		if p.Requirements == "" && len(p.Packages) == 0 {
			return fmt.Errorf("%s: requirements or packages must be set", field)
		}
		if p.Index != "" {
			if err := validateRepoURL(p.Index, field+".index"); err != nil {
				return err
			}
		}
		for _, u := range p.ExtraIndex {
			if err := validateRepoURL(u, field+".extraIndex entry"); err != nil {
				return err
			}
		}
		if len(p.BuildPackages) > 0 && inst.Apt != nil && inst.Apk != nil {
			return fmt.Errorf("%s.buildPackages cannot choose a package manager because both install.apt and install.apk are set", field)
		}
	}
	if inst.Npm != nil && inst.Npm.Manager != "" {
		switch inst.Npm.Manager {
		case "npm", "yarn", "pnpm":
		default:
			return fmt.Errorf("install.npm.manager %q is not one of npm, yarn, pnpm", inst.Npm.Manager)
		}
	}
	if inst.Uv != nil {
		for _, e := range inst.Uv.Extras {
			if err := validateRepoToken(e, "install.uv.extras entry"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMode checks an octal file mode as COPY --chmod / ADD --chmod
// accepts it.
func validateMode(mode, fieldDesc string) error {
	valid := len(mode) <= 4 && len(mode) > 0
	for _, r := range mode {
		if r < '0' || r > '7' {
			valid = false
		}
	}
	if !valid {
		return fmt.Errorf("%s %q must be an octal mode like \"0755\"", fieldDesc, mode)
	}
	return nil
}

func validateDownloads(entries []Download) error {
	dests := map[string]bool{}
	for i, d := range entries {
		where := fmt.Sprintf("download[%d]", i)
		if err := validateRepoURL(d.URL, where+" url"); err != nil {
			return err
		}
		if d.SHA256 != "" {
			if err := validateSHA256(d.SHA256, where+" sha256"); err != nil {
				return err
			}
		}
		if d.Dest == "" {
			return fmt.Errorf("%s: dest is required", where)
		}
		if d.Dest == "/" {
			return fmt.Errorf("%s: dest must not be \"/\"", where)
		}
		if err := rejectNewline(d.Dest, where+" dest"); err != nil {
			return err
		}
		if err := rejectWhitespace(d.Dest, where+" dest"); err != nil {
			return err
		}
		if err := rejectLeadingDash(d.Dest, where+" dest"); err != nil {
			return err
		}
		if dests[d.Dest] {
			return fmt.Errorf("%s: duplicate dest %q; two downloads in one stage would race to the same path", where, d.Dest)
		}
		dests[d.Dest] = true

		if d.Extract != "" {
			known := false
			for _, f := range ExtractFormats {
				if d.Extract == f {
					known = true
				}
			}
			if !known {
				return fmt.Errorf("%s: extract %q is not one of %s", where, d.Extract, strings.Join(ExtractFormats, ", "))
			}
			// Rejected rather than ignored: mode and owner describe a single
			// placed file, and an extracted archive has none by the end of
			// the stage. Accepting them would leave the Stagefile stating a
			// permission the image does not have.
			if d.Mode != "" || d.Owner != "" {
				return fmt.Errorf("%s: mode and owner do not apply with extract (they would describe the archive, which is deleted after unpacking)", where)
			}
		}
		if d.Mode != "" {
			if err := validateMode(d.Mode, where+" mode"); err != nil {
				return err
			}
		}
		if d.Owner != "" {
			if err := validateRepoToken(d.Owner, where+" owner"); err != nil {
				return err
			}
		}
	}
	return nil
}

func isCMakeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func validateCopy(entries []CopyEntry, priorNames map[string]bool) error {
	for _, e := range entries {
		if len(e.Paths) == 0 {
			return fmt.Errorf("copy from %q: paths must be non-empty", e.From)
		}
		for _, p := range e.Paths {
			// An empty entry is not a no-op: ir.Lower defaults an omitted
			// dest to Paths[0], so `paths: [""]` compiles to a COPY with a
			// missing source and a blank destination — a Dockerfile that
			// builds the wrong thing rather than one that fails to build.
			if p == "" {
				return fmt.Errorf("copy from %q: a paths entry must not be empty", e.From)
			}
			if p == "/" {
				return fmt.Errorf("copy from %q: copying \"/\" is not allowed; list the specific paths you need", e.From)
			}
		}
		if len(e.Paths) > 1 && e.Dest == "" {
			return fmt.Errorf("copy from %q: dest is required when copying more than one path", e.From)
		}
		if e.From != "local" && !priorNames[e.From] {
			return fmt.Errorf("copy from %q: not a prior stage name (and not \"local\")", e.From)
		}
		if e.Owner != "" {
			if err := validateRepoToken(e.Owner, "copy.owner"); err != nil {
				return err
			}
		}
		if e.Mode != "" {
			if err := validateMode(e.Mode, "copy.mode"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateBuildWorkdir rejects a build stage that copies its source somewhere
// the build will not run.
//
// The compiler emits no cd of its own: a build stage's RUN executes in the
// stage's workdir, and a stage without one has no WORKDIR at all, so the build
// runs in /. `copy: {dest: /app/}` with no `workdir:` therefore compiles to a
// build launched in an empty directory, which fails inside the container with
// the build tool's own "no manifest here" message — several minutes into a
// build, naming a path nothing in the Stagefile mentions.
//
// The rule is deliberately narrow: only a stage where *every* local copy lands
// at an absolute path other than / is rejected. Building at / is legal, and a
// stage that means it puts something there (a copy with no dest lands in the
// workdir, whatever it is), so a stage doing that on purpose has at least one
// copy this accepts.
func validateBuildWorkdir(s *Stage) error {
	if s.Build == nil || s.Workdir != "" || len(s.Copy) == 0 {
		return nil
	}
	var dests []string
	for _, e := range s.Copy {
		if e.From != "local" {
			continue
		}
		dest := e.Dest
		if dest == "" {
			// No dest means the path keeps its own name, relative to the
			// workdir — so the source does land where the build will run.
			return nil
		}
		if !strings.HasPrefix(dest, "/") {
			// Likewise relative to the workdir.
			return nil
		}
		if path.Clean(dest) == "/" {
			return nil
		}
		dests = append(dests, dest)
	}
	if len(dests) == 0 {
		return nil
	}
	return fmt.Errorf(
		"build runs in the stage's workdir, but this stage declares no workdir (so the build runs in /) "+
			"while its source is copied to %s — add `workdir: %s`",
		strings.Join(dests, ", "), path.Clean(dests[0]))
}

func validateBuild(b *Build) error {
	if b == nil {
		return nil
	}
	isNode := false
	switch b.Lang {
	case "rust", "go", "swift":
	case "npm", "yarn", "pnpm":
		isNode = true
	default:
		return fmt.Errorf("build.lang %q is not one of rust, go, swift, npm, yarn, pnpm", b.Lang)
	}
	if b.Profile != "" {
		if isNode {
			return fmt.Errorf("build.profile does not apply to build.lang %q (package scripts have no release/debug notion)", b.Lang)
		}
		if b.Profile != "release" && b.Profile != "debug" {
			return fmt.Errorf("build.profile %q is not one of release, debug", b.Profile)
		}
	}
	if b.Product != "" {
		if isNode {
			return fmt.Errorf("build.product does not apply to build.lang %q; use build.script", b.Lang)
		}
		// go products are package paths like "./cmd/x"; rejectLeadingDash
		// (inside validateRepoToken) still blocks flag injection.
		if err := validateRepoToken(b.Product, "build.product"); err != nil {
			return err
		}
	}
	if b.Script != "" {
		if !isNode {
			return fmt.Errorf("build.script only applies to build.lang npm, yarn, or pnpm")
		}
		if err := validateRepoToken(b.Script, "build.script"); err != nil {
			return err
		}
	}
	return nil
}

func validateHealthcheck(h *Healthcheck) error {
	if h == nil {
		return nil
	}
	if len(h.Exec) == 0 {
		return fmt.Errorf("healthcheck: exec must be non-empty")
	}
	for _, arg := range h.Exec {
		if err := rejectNewline(arg, "healthcheck.exec entry"); err != nil {
			return err
		}
	}
	for _, d := range []struct{ name, val string }{
		{"interval", h.Interval},
		{"timeout", h.Timeout},
		{"startPeriod", h.StartPeriod},
	} {
		if d.val == "" {
			continue
		}
		if _, err := time.ParseDuration(d.val); err != nil {
			return fmt.Errorf("healthcheck.%s %q is not a valid duration (e.g. \"30s\"): %w", d.name, d.val, err)
		}
	}
	if h.Retries < 0 {
		return fmt.Errorf("healthcheck.retries must not be negative (got %d)", h.Retries)
	}
	return nil
}

// rejectNewline returns an error if value contains a newline or carriage
// return. This applies everywhere, regardless of whether the field is also
// shell-quoted at codegen time (see internal/codegen's shellQuote), because
// a raw newline breaks the Dockerfile's own line-based grammar before any
// shell or builder instruction ever sees the value.
func rejectNewline(value, fieldDesc string) error {
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("%s must not contain a newline (got %q)", fieldDesc, value)
	}
	return nil
}

// rejectLeadingDash returns an error if value starts with "-", which would
// let it be parsed as a flag by the receiving package manager (pip,
// apt-get, apk) once it arrives as its own argv entry. Shell-quoting a
// value (see internal/codegen's shellQuote) stops the *shell* from
// treating it specially, but does nothing to stop the *program* pip/apt/apk
// is invoking from treating a leading "-" as an option rather than a name.
func rejectLeadingDash(value, fieldDesc string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s must not start with \"-\" (got %q); it would be parsed as a flag, not a package name", fieldDesc, value)
	}
	return nil
}

// rejectWhitespace returns an error if value contains any whitespace
// character (space, tab, or any other rune unicode.IsSpace treats as
// whitespace — including \v and \f, both of which BuildKit's Dockerfile
// tokenizer also splits COPY arguments on). This checks the property
// ("is this rune a separator?") rather than an enumerated list of
// characters, because an enumerated list only ever covers the bytes
// someone already thought to test.
func rejectWhitespace(value, fieldDesc string) error {
	if strings.ContainsFunc(value, unicode.IsSpace) {
		return fmt.Errorf("%s must not contain whitespace (got %q)", fieldDesc, value)
	}
	return nil
}

func validateNoInjection(s *Stage) error {
	if err := rejectNewline(s.Name, "name"); err != nil {
		return err
	}
	if err := rejectNewline(s.From, "from"); err != nil {
		return err
	}
	if err := rejectNewline(s.User, "user"); err != nil {
		return err
	}
	if s.Install != nil {
		if s.Install.Apt != nil {
			for _, p := range s.Install.Apt.Packages {
				if err := rejectNewline(p, "install.apt.packages entry"); err != nil {
					return err
				}
				if err := rejectLeadingDash(p, "install.apt.packages entry"); err != nil {
					return err
				}
			}
		}
		if s.Install.Apk != nil {
			for _, p := range s.Install.Apk.Packages {
				if err := rejectNewline(p, "install.apk.packages entry"); err != nil {
					return err
				}
				if err := rejectLeadingDash(p, "install.apk.packages entry"); err != nil {
					return err
				}
			}
		}
		for _, pip := range s.Install.Pip {
			if err := rejectNewline(pip.Requirements, "install.pip.requirements"); err != nil {
				return err
			}
			if err := rejectWhitespace(pip.Requirements, "install.pip.requirements"); err != nil {
				return err
			}
			if err := rejectLeadingDash(pip.Requirements, "install.pip.requirements"); err != nil {
				return err
			}
			for _, p := range pip.Packages {
				if err := rejectNewline(p, "install.pip.packages entry"); err != nil {
					return err
				}
				if err := rejectLeadingDash(p, "install.pip.packages entry"); err != nil {
					return err
				}
			}
			for _, p := range pip.BuildPackages {
				if err := rejectNewline(p, "install.pip.buildPackages entry"); err != nil {
					return err
				}
				if err := rejectLeadingDash(p, "install.pip.buildPackages entry"); err != nil {
					return err
				}
			}
		}
	}
	for _, c := range s.Copy {
		if err := rejectNewline(c.From, "copy.from"); err != nil {
			return err
		}
		if err := rejectWhitespace(c.From, "copy.from"); err != nil {
			return err
		}
		if err := rejectLeadingDash(c.From, "copy.from"); err != nil {
			return err
		}
		if err := rejectNewline(c.Dest, "copy.dest"); err != nil {
			return err
		}
		if err := rejectWhitespace(c.Dest, "copy.dest"); err != nil {
			return err
		}
		if err := rejectLeadingDash(c.Dest, "copy.dest"); err != nil {
			return err
		}
		for _, p := range c.Paths {
			if err := rejectNewline(p, "copy.paths entry"); err != nil {
				return err
			}
			if err := rejectWhitespace(p, "copy.paths entry"); err != nil {
				return err
			}
			if err := rejectLeadingDash(p, "copy.paths entry"); err != nil {
				return err
			}
		}
	}
	if s.Entrypoint != nil {
		for _, arg := range s.Entrypoint.Exec {
			if err := rejectNewline(arg, "entrypoint.exec entry"); err != nil {
				return err
			}
		}
		if err := rejectNewline(s.Entrypoint.Source, "entrypoint.source"); err != nil {
			return err
		}
	}
	for _, arg := range s.Cmd {
		if err := rejectNewline(arg, "cmd entry"); err != nil {
			return err
		}
	}
	return nil
}
