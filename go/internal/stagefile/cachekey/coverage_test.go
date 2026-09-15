package cachekey

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
)

// Every field keyedPayloadFields pins must be able to change a key on its
// own. A field that is listed, wired through Lower and codegen, but never
// actually hashed is the exact defect the field guard exists to catch — and
// the guard cannot catch it, because listing a field is not the same as
// hashing it. These tests close that gap: each case mutates one field of an
// otherwise identical node and asserts the key moves.
//
// The graphs here are hand-built rather than lowered, so that one field can
// be varied in isolation — a spec-level edit often changes two at once.

func keyOf(t *testing.T, n ir.Node, in Inputs) string {
	t.Helper()
	g := &ir.Graph{Nodes: []ir.Node{n}}
	k, err := Key(g, 0, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return k
}

func imageInputs() Inputs {
	return Inputs{Images: map[string]string{"debian:12": "sha256:aaa", "other:1": "sha256:bbb"}}
}

func TestImageFieldsChangeTheKey(t *testing.T) {
	base := ir.Node{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "debian:12"}}
	baseKey := keyOf(t, base, imageInputs())

	cases := []struct {
		name string
		op   ir.ImageOp
	}{
		{"platform", ir.ImageOp{Ref: "debian:12", Platform: "linux/arm64"}},
		{"args", ir.ImageOp{Ref: "debian:12", Args: map[string]string{"V": "1"}}},
		{"env", ir.ImageOp{Ref: "debian:12", Env: map[string]string{"MODE": "prod"}}},
		{"workdir", ir.ImageOp{Ref: "debian:12", Workdir: "/srv"}},
		{"unpinned", ir.ImageOp{Ref: "debian:12", Unpinned: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := tc.op
			if got := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &op}, imageInputs()); got == baseKey {
				t.Fatalf("changing %s did not change the key", tc.name)
			}
		})
	}
}

// An env map's key must not depend on Go's randomized map iteration order,
// and two maps that differ only in declaration order are one build.
func TestImageEnvKeyIsOrderIndependent(t *testing.T) {
	a := ir.ImageOp{Ref: "debian:12", Env: map[string]string{"A": "1", "B": "2", "C": "3"}}
	b := ir.ImageOp{Ref: "debian:12", Env: map[string]string{"C": "3", "B": "2", "A": "1"}}
	for range 8 {
		ka := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &a}, imageInputs())
		kb := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &b}, imageInputs())
		if ka != kb {
			t.Fatalf("same env keyed differently across map orderings:\n %s\n %s", ka, kb)
		}
	}
}

// An unpinned image keys off its ref, so two different unpinned refs differ —
// but it cannot collide with a pinned node whose digest happens to equal a
// ref string, which is what the separate "unpinned" tag buys.
func TestUnpinnedImageKeysOffItsRef(t *testing.T) {
	in := imageInputs()
	a := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "local/a:dev", Unpinned: true}}, in)
	b := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "local/b:dev", Unpinned: true}}, in)
	if a == b {
		t.Fatal("two different unpinned refs keyed identically")
	}

	collide := Inputs{Images: map[string]string{"x": "local/a:dev"}}
	pinned := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "x"}}, collide)
	unpinned := keyOf(t, ir.Node{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "local/a:dev", Unpinned: true}}, collide)
	if pinned == unpinned {
		t.Fatal("a pinned node whose digest equals an unpinned node's ref collided with it")
	}
}

func TestFetchFieldsChangeTheKey(t *testing.T) {
	base := ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/x.bin", Checksum: "sha256:aaa"}
	baseKey := keyOf(t, ir.Node{Kind: ir.OpFetch, Fetch: &base}, Inputs{})

	cases := []struct {
		name string
		op   ir.FetchOp
	}{
		{"url", ir.FetchOp{URL: "https://example.test/y.bin", Dest: "/x.bin", Checksum: "sha256:aaa"}},
		{"dest", ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/y.bin", Checksum: "sha256:aaa"}},
		{"checksum", ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/x.bin", Checksum: "sha256:bbb"}},
		{"mode", ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/x.bin", Checksum: "sha256:aaa", Mode: "0755"}},
		{"owner", ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/x.bin", Checksum: "sha256:aaa", Owner: "1000:1000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := tc.op
			if got := keyOf(t, ir.Node{Kind: ir.OpFetch, Fetch: &op}, Inputs{}); got == baseKey {
				t.Fatalf("changing %s did not change the key", tc.name)
			}
		})
	}
}

func TestCopyOwnerAndModeChangeTheKey(t *testing.T) {
	in := Inputs{Files: map[string]string{"app.py": "sha256:app"}}
	base := ir.CopyOp{FromLocal: true, Paths: []string{"app.py"}, Dest: "app.py"}
	baseKey := keyOf(t, ir.Node{Kind: ir.OpCopy, Copy: &base}, in)

	owner := base
	owner.Owner = "1000:1000"
	if keyOf(t, ir.Node{Kind: ir.OpCopy, Copy: &owner}, in) == baseKey {
		t.Error("changing copy owner did not change the key")
	}
	mode := base
	mode.Mode = "0755"
	if keyOf(t, ir.Node{Kind: ir.OpCopy, Copy: &mode}, in) == baseKey {
		t.Error("changing copy mode did not change the key")
	}
}

func execKey(t *testing.T, x ir.ExecOp, in Inputs) string {
	t.Helper()
	return keyOf(t, ir.Node{Kind: ir.OpExec, Exec: &x}, in)
}

func TestExecPayloadFieldsChangeTheKey(t *testing.T) {
	uvFiles := []string{"pyproject.toml", "uv.lock"}
	in := Inputs{Files: map[string]string{
		"requirements.txt":  "sha256:reqs",
		"package.json":      "sha256:pkg",
		"package-lock.json": "sha256:lock",
		"pyproject.toml":    "sha256:pyproject",
		"uv.lock":           "sha256:uvlock",
	}}

	cases := []struct {
		name string
		a, b ir.ExecOp
	}{
		{
			"apt repositories",
			ir.ExecOp{Recipe: ir.RecipeApt, Apt: &ir.AptParams{Packages: []string{"curl"}}},
			ir.ExecOp{Recipe: ir.RecipeApt, Apt: &ir.AptParams{Packages: []string{"curl"}, Repositories: []ir.AptRepository{{
				Name: "ros2", URL: "http://packages.ros.org/ros2/ubuntu",
				Suites: []string{"jammy"}, Components: []string{"main"},
				KeyURL: "https://example.test/ros.key", KeySHA256: "abc",
			}}}},
		},
		{
			"apt repository key digest",
			ir.ExecOp{Recipe: ir.RecipeApt, Apt: &ir.AptParams{Repositories: []ir.AptRepository{{Name: "r", KeySHA256: "abc"}}}},
			ir.ExecOp{Recipe: ir.RecipeApt, Apt: &ir.AptParams{Repositories: []ir.AptRepository{{Name: "r", KeySHA256: "def"}}}},
		},
		{
			"apk cache",
			ir.ExecOp{Recipe: ir.RecipeApk, Apk: &ir.ApkParams{Packages: []string{"musl-dev"}}},
			ir.ExecOp{Recipe: ir.RecipeApk, Apk: &ir.ApkParams{Packages: []string{"musl-dev"}, Cache: true}},
		},
		{
			"apk repositories",
			ir.ExecOp{Recipe: ir.RecipeApk, Apk: &ir.ApkParams{Packages: []string{"musl-dev"}}},
			ir.ExecOp{Recipe: ir.RecipeApk, Apk: &ir.ApkParams{Packages: []string{"musl-dev"}, Repositories: []string{"https://example.test/edge"}}},
		},
		{
			"cmake commit",
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/x"}},
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "b2", Root: "/tmp/x"}},
		},
		{
			"cmake defines",
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/x"}},
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/x", Defines: map[string]string{"T": "OFF"}}},
		},
		{
			"cmake jobs",
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/x"}},
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/x", Jobs: 4}},
		},
		{
			"cmake root",
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/x"}},
			ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "a1", Root: "/tmp/y"}},
		},
		{
			"pip index",
			ir.ExecOp{Recipe: ir.RecipePip, Pip: &ir.PipParams{Packages: []string{"torch"}}},
			ir.ExecOp{Recipe: ir.RecipePip, Pip: &ir.PipParams{Packages: []string{"torch"}, Index: "https://pypi.jetson.example/simple"}},
		},
		{
			"pip extra index",
			ir.ExecOp{Recipe: ir.RecipePip, Pip: &ir.PipParams{Packages: []string{"torch"}}},
			ir.ExecOp{Recipe: ir.RecipePip, Pip: &ir.PipParams{Packages: []string{"torch"}, ExtraIndex: []string{"https://extra.example/simple"}}},
		},
		{
			"npm production",
			ir.ExecOp{Recipe: ir.RecipeNpm, Npm: &ir.NpmParams{Manager: "npm", Manifest: "package.json", Lockfile: "package-lock.json"}},
			ir.ExecOp{Recipe: ir.RecipeNpm, Npm: &ir.NpmParams{Manager: "npm", Manifest: "package.json", Lockfile: "package-lock.json", Production: true}},
		},
		{
			"uv dev",
			ir.ExecOp{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: uvFiles}},
			ir.ExecOp{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: uvFiles, Dev: true}},
		},
		{
			"uv extras",
			ir.ExecOp{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: uvFiles}},
			ir.ExecOp{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: uvFiles, Extras: []string{"gpu"}}},
		},
		{
			"extract format",
			ir.ExecOp{Recipe: ir.RecipeExtract, Extract: &ir.ExtractParams{Archive: "/tmp/a.tar.gz", Dest: "/m", Format: "tar.gz"}},
			ir.ExecOp{Recipe: ir.RecipeExtract, Extract: &ir.ExtractParams{Archive: "/tmp/a.tar.gz", Dest: "/m", Format: "zip"}},
		},
		{
			"cuda collect libdir",
			ir.ExecOp{Recipe: ir.RecipeCUDACollect, CUDACollect: &ir.CUDACollectParams{LibDir: "/opt/a", ConfPath: "/etc/c"}},
			ir.ExecOp{Recipe: ir.RecipeCUDACollect, CUDACollect: &ir.CUDACollectParams{LibDir: "/opt/b", ConfPath: "/etc/c"}},
		},
		{
			"build product",
			ir.ExecOp{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "swift", Profile: "release"}},
			ir.ExecOp{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "swift", Profile: "release", Product: "Server"}},
		},
		{
			"build script",
			ir.ExecOp{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "npm", Script: "build"}},
			ir.ExecOp{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "npm", Script: "bundle"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if execKey(t, tc.a, in) == execKey(t, tc.b, in) {
				t.Fatalf("changing %s did not change the key", tc.name)
			}
		})
	}
}

// uv reads pyproject.toml and uv.lock, and neither the Dockerfile nor the
// image lockfile varies with their contents — so if the key did not hash
// them, editing a dependency would change nothing any cache could see. This
// is the same defect npm's package.json had.
func TestUvHashesItsLockFiles(t *testing.T) {
	x := ir.ExecOp{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: []string{"pyproject.toml", "uv.lock"}}}
	base := Inputs{Files: map[string]string{"pyproject.toml": "sha256:a", "uv.lock": "sha256:b"}}
	edited := Inputs{Files: map[string]string{"pyproject.toml": "sha256:a", "uv.lock": "sha256:CHANGED"}}
	if execKey(t, x, base) == execKey(t, x, edited) {
		t.Fatal("editing uv.lock did not change the key")
	}
}

func TestUvRefusesAMissingFileDigest(t *testing.T) {
	g := &ir.Graph{Nodes: []ir.Node{{Kind: ir.OpExec, Exec: &ir.ExecOp{
		Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: []string{"pyproject.toml", "uv.lock"}},
	}}}}
	if _, err := Key(g, 0, Inputs{Files: map[string]string{"pyproject.toml": "sha256:a"}}); err == nil {
		t.Fatal("Key succeeded with no digest for uv.lock")
	}
}

func TestKeyRefusesANilFetchPayload(t *testing.T) {
	g := &ir.Graph{Nodes: []ir.Node{{Kind: ir.OpFetch}}}
	if _, err := Key(g, 0, Inputs{}); err == nil {
		t.Fatal("Key succeeded on a fetch node with a nil payload")
	}
}
