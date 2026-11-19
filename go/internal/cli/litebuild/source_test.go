package litebuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRepo is a local wendy-lite repository, which the tests clone through a
// file:// URL, as the CLI clones GitHub.
type testRepo struct {
	t   *testing.T
	dir string
	url string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := filepath.Join(t.TempDir(), "wendy-lite")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, dir: dir, url: "file://" + filepath.ToSlash(dir)}
	r.git("init", "--quiet")
	r.git("symbolic-ref", "HEAD", "refs/heads/main")
	// As GitHub: fetches of a commit hash, and without the file contents.
	r.git("config", "uploadpack.allowAnySHA1InWant", "true")
	r.git("config", "uploadpack.allowFilter", "true")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	return r.gitIn(r.dir, args...)
}

// gitIn runs git in dir, for example a clone of the repository, and
// returns its output.
func (r *testRepo) gitIn(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit commits file.txt with content and returns the full commit hash.
func (r *testRepo) commit(content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "file.txt"), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "file.txt")
	r.git("commit", "--quiet", "-m", content)
	return r.git("rev-parse", "HEAD")
}

func TestResolveRef(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit("one")
	r.git("tag", "light")
	r.git("tag", "-a", "-m", "annotated", "annotated")
	r.git("tag", "same")
	r.git("checkout", "--quiet", "-b", "feature/x")
	second := r.commit("two")
	r.git("branch", "same")

	for _, tc := range []struct{ ref, commit string }{
		{"main", first},
		{"feature/x", second},
		{"light", first},
		// The commit, not the tag object.
		{"annotated", first},
		// A branch wins over a tag, as with git clone --branch.
		{"same", second},
	} {
		commit, err := resolveRef(context.Background(), r.url, tc.ref)
		if err != nil {
			t.Errorf("%s: %v", tc.ref, err)
			continue
		}
		if commit != tc.commit {
			t.Errorf("%s = %s, want %s", tc.ref, commit, tc.commit)
		}
	}

	// "x" is only the end of "feature/x".
	for _, ref := range []string{"does-not-exist", "x"} {
		if _, err := resolveRef(context.Background(), r.url, ref); err == nil || !strings.Contains(err.Error(), `"`+ref+`"`) || strings.Contains(err.Error(), "full hash") {
			t.Errorf("%s: %v, want an error naming the ref", ref, err)
		}
	}
	// What can be an abbreviated commit hash: git cannot fetch it.
	if _, err := resolveRef(context.Background(), r.url, first[:7]); err == nil || !strings.Contains(err.Error(), "full hash") {
		t.Errorf("abbreviated hash: %v, want the full hash hint", err)
	}
}

func TestResolveSource(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	if _, err := ResolveSource(ctx, r.url, false, ""); err == nil || !strings.Contains(err.Error(), "no release tag") {
		t.Errorf("without release: %v, want an error", err)
	}

	v09 := r.commit("0.9")
	r.git("tag", "v0.9.0")
	v010 := r.commit("0.10")
	r.git("tag", "-a", "-m", "annotated", "v0.10.0")
	night := r.commit("nightly")
	r.git("tag", "nightly")
	r.git("tag", "v1.0.0-rc1")
	r.git("checkout", "--quiet", "-b", "feature/x")
	branch := r.commit("branch")

	for _, tc := range []struct {
		name    string
		nightly bool
		version string
		want    Source
		label   string
	}{
		// Numbers, not texts: 10 > 9. Not the pre-release, not nightly. The
		// annotated tag gives its commit.
		{name: "latest", want: Source{"0.10.0", "0.10.0", v010}, label: "0.10.0"},
		{name: "release", version: "0.9.0", want: Source{"0.9.0", "0.9.0", v09}, label: "0.9.0"},
		{name: "release tag", version: "v0.9.0", want: Source{"0.9.0", "0.9.0", v09}, label: "0.9.0"},
		{name: "nightly", nightly: true, want: Source{"nightly", night, night}, label: "nightly (" + night[:7] + ")"},
		{name: "nightly wins", nightly: true, version: "0.9.0", want: Source{"nightly", night, night}},
		{name: "branch", version: "feature/x", want: Source{"feature/x", branch, branch}, label: "feature/x (" + branch[:7] + ")"},
		{name: "other tag", version: "v1.0.0-rc1", want: Source{"v1.0.0-rc1", night, night}},
		{name: "hash", version: v09, want: Source{v09, v09, v09}, label: v09[:7]},
		{name: "hash in upper case", version: strings.ToUpper(v09), want: Source{v09, v09, v09}},
	} {
		got, err := ResolveSource(ctx, r.url, tc.nightly, tc.version)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, got, tc.want)
		}
		if tc.label != "" && got.String() != tc.label {
			t.Errorf("%s: String() = %q, want %q", tc.name, got.String(), tc.label)
		}
	}
	if ref := (Source{"0.9.0", "0.9.0", v09}).Ref(); ref != "v0.9.0" {
		t.Errorf("Ref of a release = %q, want v0.9.0", ref)
	}
	if ref := (Source{"nightly", night, night}).Ref(); ref != night {
		t.Errorf("Ref of nightly = %q, want %s", ref, night)
	}

	// A hash is not looked up: git ls-remote lists only branches and tags.
	unknown := strings.Repeat("ab", 20)
	if got, err := ResolveSource(ctx, r.url, false, unknown); err != nil || got.Commit != unknown {
		t.Errorf("unknown hash = %+v, %v; want it as is", got, err)
	}
	if _, err := ResolveSource(ctx, r.url, false, "1.2.3"); err == nil || !strings.Contains(err.Error(), "Wendy Lite 1.2.3 does not exist") || !strings.Contains(err.Error(), "v1.2.3") {
		t.Errorf("unknown release: %v, want an error naming it", err)
	}
	if _, err := ResolveSource(ctx, r.url, false, "does-not-exist"); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("unknown branch: %v, want an error naming it", err)
	}
	r.git("tag", "-d", "nightly")
	if _, err := ResolveSource(ctx, r.url, true, ""); err == nil || !strings.Contains(err.Error(), "no nightly tag") {
		t.Errorf("without nightly tag: %v, want an error", err)
	}
}

func TestPrepareSources(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	first := r.commit("one")
	r.git("tag", "v1.2.3")
	second := r.commit("two")
	root := t.TempDir()
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }
	prepare := func(root string, src Source) string {
		t.Helper()
		logs = nil
		dir, err := PrepareSources(ctx, root, r.url, src, logf)
		if err != nil {
			t.Fatalf("PrepareSources(%s): %v", src, err)
		}
		if want := filepath.Join(root, BuildFolderName(src.Commit)); dir != want {
			t.Fatalf("PrepareSources(%s) = %s, want %s", src, dir, want)
		}
		if head := r.gitIn(dir, "rev-parse", "HEAD"); head != src.Commit {
			t.Fatalf("PrepareSources(%s): HEAD %s, want %s", src, head, src.Commit)
		}
		return dir
	}
	content := func(dir string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	logged := func(prefix string) bool {
		for _, l := range logs {
			if strings.HasPrefix(l, prefix) {
				return true
			}
		}
		return false
	}
	version := func(dir string) string {
		t.Helper()
		v, err := SourceVersion(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// A commit that is not the tip of a branch: fetched by its hash, into a
	// new folder named after it.
	old := Source{Name: first, Version: first, Commit: first}
	dir := prepare(root, old)
	if content(dir) != "one" || !logged("Cloning") {
		t.Errorf("content %q, logs %q", content(dir), logs)
	}
	// Only the commit: the tag of a release is not fetched.
	if v := version(dir); v != first {
		t.Errorf("version %q, want %s", v, first)
	}

	// The same commit: the folder and its build stay, and it is marked as
	// used now.
	marker := filepath.Join(dir, "build", "marker")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, past, past); err != nil {
		t.Fatal(err)
	}
	if again := prepare(root, old); again != dir || !logged("Reusing") {
		t.Errorf("folder %s, logs %q; want the reuse of %s", again, logs, dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the build of the reused folder is lost: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.ModTime().After(past) {
		t.Errorf("the modification time of the reused folder is not updated: %v", err)
	}

	// A release that the reused folder has no tag for: the tag is fetched,
	// and the version is then that of the tag.
	release := Source{Name: "1.2.3", Version: "1.2.3", Commit: first}
	if again := prepare(root, release); again != dir {
		t.Errorf("folder %s, want the reuse of %s", again, dir)
	}
	if v := version(dir); v != "1.2.3" {
		t.Errorf("reused release: version %q, want 1.2.3", v)
	}
	// A new folder of a release has the tag.
	if dir := prepare(t.TempDir(), release); version(dir) != "1.2.3" {
		t.Errorf("new release: version %q, want 1.2.3", version(dir))
	}

	// A new commit: a second folder.
	tip := Source{Name: "main", Version: second, Commit: second}
	dir2 := prepare(root, tip)
	if dir2 == dir || content(dir2) != "two" || content(dir) != "one" {
		t.Errorf("folders %s (%q) and %s (%q)", dir, content(dir), dir2, content(dir2))
	}

	// A folder that is not a clone is cloned again.
	if err := os.RemoveAll(filepath.Join(dir2, ".git")); err != nil {
		t.Fatal(err)
	}
	if again := prepare(root, tip); again != dir2 || !logged("The build folder") || content(dir2) != "two" {
		t.Errorf("folder %s, logs %q; want a new clone in %s", again, logs, dir2)
	}

	// A failed clone leaves no folder.
	failed := t.TempDir()
	unknown := strings.Repeat("f", 40)
	if _, err := PrepareSources(ctx, failed, r.url, Source{Name: unknown, Version: unknown, Commit: unknown}, logf); err == nil {
		t.Error("the clone of an unknown commit succeeded")
	}
	if entries, _ := os.ReadDir(failed); len(entries) != 0 {
		t.Errorf("the failed clone left %d entries", len(entries))
	}
}

func TestSourceVersion(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	commit := r.commit("one")
	for _, tc := range []struct{ tag, want string }{
		{"", commit},
		{"nightly", commit},
		{"v1.2.3", "1.2.3"},
		// Numbers, not texts: 10 > 2.
		{"v1.10.0", "1.10.0"},
		{"v2.0.0-rc1", "1.10.0"},
		{"v1.9.9", "1.10.0"},
	} {
		if tc.tag != "" {
			r.git("tag", tc.tag)
		}
		if got, err := SourceVersion(ctx, r.dir); err != nil || got != tc.want {
			t.Errorf("with the tag %q: %q, %v; want %q", tc.tag, got, err, tc.want)
		}
	}
}

func TestConfigureReason(t *testing.T) {
	dir := t.TempDir()
	const profile = `{"board":"seeed_xiao","app_type":"native"}`
	if reason, err := ConfigureReason(dir, profile, "1.2.3"); err != nil || !strings.Contains(reason, "not configured") {
		t.Errorf("without sdkconfig: %q, %v", reason, err)
	}
	sdkconfig := "# comment\n" + BuildProfileLine(profile) + "\n" + VersionOption + `="1.2.3"` + "\nCONFIG_OTHER=y\n"
	if err := os.WriteFile(filepath.Join(dir, "sdkconfig"), []byte(sdkconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		profile, version, want string
	}{
		{profile, "1.2.3", ""},
		// The same map: the order and the quoting do not count.
		{`{"app_type": native, "board": "seeed_xiao"}`, "1.2.3", ""},
		{`{"board":"seeed_xiao","app_type":"wasm"}`, "1.2.3", "answers"},
		{`{"board":"seeed_xiao"}`, "1.2.3", "answers"},
		{profile, "1.2.4", `"1.2.4"`},
	} {
		reason, err := ConfigureReason(dir, tc.profile, tc.version)
		if err != nil {
			t.Fatal(err)
		}
		if (tc.want == "") != (reason == "") || !strings.Contains(reason, tc.want) {
			t.Errorf("%s %s: %q, want %q", tc.profile, tc.version, reason, tc.want)
		}
	}
}

func TestPruneBuildFolders(t *testing.T) {
	const (
		a = "aaaaaaaaaaaa"
		b = "bbbbbbbbbbbb"
		c = "cccccccccccc"
		d = "dddddddddddd"
	)
	full := func(name string) string { return name + strings.Repeat("0", 28) }
	// setup makes the build folders in a new root, the oldest first, and
	// entries that are not build folders.
	setup := func(t *testing.T, folders ...string) string {
		t.Helper()
		root := t.TempDir()
		for i, name := range folders {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Join(path, "build"), 0o755); err != nil {
				t.Fatal(err)
			}
			used := time.Now().Add(-time.Duration(len(folders)-i) * time.Hour)
			if err := os.Chtimes(path, used, used); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"notes", "ABCDEFABCDEF", cloneStagingPrefix + "123"} {
			if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"answers.json", ".lock"} {
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	for _, tc := range []struct {
		name    string
		folders []string // oldest first
		current string
		want    []string // the build folders that stay
	}{
		{"new commit", []string{a, b, c}, d, []string{c}},
		{"reused old folder", []string{a, b, c}, a, []string{a, c}},
		{"reused last folder", []string{a, b, c}, c, []string{b, c}},
		{"two folders", []string{a, b}, a, []string{a, b}},
		{"one folder", []string{a}, d, []string{a}},
		{"no folder", nil, d, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := setup(t, tc.folders...)
			var removed int
			logf := func(string, ...any) { removed++ }
			warnf := func(format string, args ...any) { t.Errorf("warning: "+format, args...) }
			PruneBuildFolders(root, full(tc.current), logf, warnf)

			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			var got, others []string
			for _, e := range entries {
				if isBuildFolderName(e.Name()) {
					got = append(got, e.Name())
				} else {
					others = append(others, e.Name())
				}
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("build folders %v, want %v", got, tc.want)
			}
			if removed != len(tc.folders)-len(tc.want) {
				t.Errorf("%d removals reported, want %d", removed, len(tc.folders)-len(tc.want))
			}
			// The staging folder goes, the rest stays.
			if strings.Join(others, " ") != ".lock ABCDEFABCDEF answers.json notes" {
				t.Errorf("other entries %v", others)
			}
		})
	}

	// No root: nothing to do, no warning.
	PruneBuildFolders(filepath.Join(t.TempDir(), "missing"), full(a), func(string, ...any) {}, func(format string, args ...any) {
		t.Errorf("warning: "+format, args...)
	})
}
