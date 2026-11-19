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
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
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

	for _, tc := range []struct {
		ref    string
		commit string
		tag    bool
	}{
		{"main", first, false},
		{"feature/x", second, false},
		{"light", first, true},
		// The commit, not the tag object.
		{"annotated", first, true},
		// A branch wins over a tag, as with git clone --branch.
		{"same", second, false},
	} {
		commit, tag, err := ResolveRef(context.Background(), r.url, tc.ref)
		if err != nil {
			t.Errorf("%s: %v", tc.ref, err)
			continue
		}
		if commit != tc.commit || tag != tc.tag {
			t.Errorf("%s = %s tag %v, want %s tag %v", tc.ref, commit, tag, tc.commit, tc.tag)
		}
	}

	// "x" is only the end of "feature/x".
	for _, ref := range []string{"does-not-exist", "x"} {
		if _, _, err := ResolveRef(context.Background(), r.url, ref); err == nil || !strings.Contains(err.Error(), `"`+ref+`"`) {
			t.Errorf("%s: %v, want an error naming the ref", ref, err)
		}
	}
}

func TestPrepareSources(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	first := r.commit("one")
	root := t.TempDir()
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }
	prepare := func(root, ref, commit string, tag bool) (string, string) {
		t.Helper()
		logs = nil
		dir, head, err := PrepareSources(ctx, root, r.url, ref, commit, tag, logf)
		if err != nil {
			t.Fatalf("PrepareSources(%s, %s): %v", ref, ShortVersion(commit), err)
		}
		return dir, head
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

	// A new folder, named after the commit.
	dir, head := prepare(root, "main", first, false)
	if want := filepath.Join(root, BuildFolderName(first)); dir != want || head != first {
		t.Fatalf("folder %s at %s, want %s at %s", dir, head, want, first)
	}
	if content(dir) != "one" || !logged("Cloning") {
		t.Errorf("content %q, logs %q", content(dir), logs)
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
	if again, _ := prepare(root, "main", first, false); again != dir || !logged("Reusing") {
		t.Errorf("folder %s, logs %q; want the reuse of %s", again, logs, dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the build of the reused folder is lost: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.ModTime().After(past) {
		t.Errorf("the modification time of the reused folder is not updated: %v", err)
	}

	// A new commit: a second folder.
	second := r.commit("two")
	dir2, _ := prepare(root, "main", second, false)
	if dir2 == dir || content(dir2) != "two" || content(dir) != "one" {
		t.Errorf("folders %s (%q) and %s (%q)", dir, content(dir), dir2, content(dir2))
	}

	// A folder that is not a clone is cloned again.
	if err := os.RemoveAll(filepath.Join(dir2, ".git")); err != nil {
		t.Fatal(err)
	}
	if again, _ := prepare(root, "main", second, false); again != dir2 || !logged("The build folder") || content(dir2) != "two" {
		t.Errorf("folder %s, logs %q; want a new clone in %s", again, logs, dir2)
	}

	// A tag that the reused folder does not have is fetched: the version is
	// then that of the tag.
	r.git("tag", "v1.2.3")
	commit, tag, err := ResolveRef(ctx, r.url, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := prepare(root, "v1.2.3", commit, tag); again != dir2 {
		t.Errorf("folder %s, want the reuse of %s", again, dir2)
	}
	if v, err := SourceVersion(ctx, dir2); err != nil || v != "1.2.3" {
		t.Errorf("SourceVersion = %q, %v; want 1.2.3", v, err)
	}

	// The ref moved after ResolveRef: the folder of the cloned commit.
	moved := t.TempDir()
	if dir, head := prepare(moved, "main", first, false); head != second || dir != filepath.Join(moved, BuildFolderName(second)) || !logged("wendy-lite %s moved") {
		t.Errorf("folder %s at %s, logs %q; want the folder of %s", dir, head, logs, second)
	}

	// A failed clone leaves no folder.
	failed := t.TempDir()
	if _, _, err := PrepareSources(ctx, failed, r.url, "does-not-exist", strings.Repeat("f", 40), false, logf); err == nil {
		t.Error("the clone of an unknown ref succeeded")
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
