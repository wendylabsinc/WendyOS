package cachekey

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
)

// keyedPayloadFields pins the exported fields of every ir payload the key
// covers. It is the guard for a defect this branch actually shipped: npm's
// package.json was copied into the image and allowlisted as a build input,
// but never hashed, so editing it produced an identical key and a different
// rootfs. Nothing failed, because adding a field to an ir payload and wiring
// it through Lower and codegen changes the filesystem without touching any
// test that looks at keys.
//
// Listing a field here is not the same as hashing it. Stage-level config
// (ir.Stage.Entrypoint, ir.Stage.User) is deliberately outside the key and
// deliberately outside this list; ir.ImageOp.Ref is listed but intentionally
// excluded from the hash, because two tags resolving to one digest are one
// rootfs. The list's job is to force the question to be asked.
var keyedPayloadFields = map[string][]string{
	"AptParams":          {"Packages", "Recommends", "Repositories", "Base"},
	"AptRepository":      {"Name", "URL", "Suites", "Components", "KeyURL", "KeySHA256", "KeyFormat"},
	"ApkParams":          {"Packages", "Cache", "Repositories"},
	"CMakeParams":        {"Repository", "Commit", "Prefix", "BuildType", "Defines", "Jobs", "Root"},
	"PipParams":          {"Requirements", "Packages", "BuildPackages", "Index", "ExtraIndex", "Root", "Target"},
	"PipBootstrapParams": {"Manager", "Packages", "AptRepositories", "ApkRepositories", "AptBase"},
	"NpmParams":          {"Manager", "Manifest", "Lockfile", "Production"},
	"UvParams":           {"Extras", "Dev", "Files"},
	"ExtractParams":      {"Archive", "Dest", "Format"},
	// LibDir and ConfPath are both hashed. They come from the pinned GPU
	// profile rather than the Stagefile, so in practice they change only
	// when the profile does — but they are paths the built rootfs actually
	// contains, so they belong in the key rather than being excluded as
	// rendering detail.
	"CUDACollectParams": {"LibDir", "ConfPath"},
	"BuildParams":       {"Lang", "Profile", "Product", "Script", "From", "CacheScope"},
	"CopyOp":            {"FromLocal", "Link", "Paths", "Dest", "Owner", "Mode"},
	"FetchOp":           {"URL", "Dest", "Checksum", "Mode", "Owner"},
	// Ref is listed but hashed only for an unpinned image: for a pinned one
	// the resolved digest stands in for it, because two tags resolving to
	// one digest are one rootfs. Unpinned itself is hashed by selecting
	// between those two encodings.
	"ImageOp": {"Ref", "FromStage", "Unpinned", "Platform", "Args", "Env", "Workdir"},
	"ExecOp": {"Recipe", "Apt", "Apk", "CMake", "Pip", "PipBootstrap", "Npm", "Uv",
		"Extract", "CUDACollect", "Build"},
	"Recipe": {"Name", "Version"},
}

// TestIRPayloadFieldsAreAccountedFor fails whenever an ir payload gains or
// loses an exported field.
//
// If this test is failing, do not just update the list. First decide whether
// the new field changes the filesystem the node produces:
//
//   - If it does, it belongs in the cache key. Hash it in cachekey.go
//     (writeExec or write), add a test asserting two Inputs differing only in
//     that field key differently, bump keyFormatVersion or the relevant
//     ir.Recipe.Version, and re-harvest the golden corpus. Then add the field
//     here.
//   - If it does not — it is image config, or a rendering hint with no effect
//     on the resulting rootfs — add it here with a comment saying why it is
//     excluded, the way ir.Stage.Entrypoint and ir.Stage.User are.
//
// Skipping that decision is how a key silently stops covering a build input.
func TestIRPayloadFieldsAreAccountedFor(t *testing.T) {
	payloads := []any{
		ir.AptParams{},
		ir.AptRepository{},
		ir.ApkParams{},
		ir.CMakeParams{},
		ir.PipParams{},
		ir.PipBootstrapParams{},
		ir.NpmParams{},
		ir.UvParams{},
		ir.ExtractParams{},
		ir.CUDACollectParams{},
		ir.BuildParams{},
		ir.CopyOp{},
		ir.FetchOp{},
		ir.ImageOp{},
		ir.ExecOp{},
		ir.Recipe{},
	}

	seen := map[string]bool{}
	for _, p := range payloads {
		typ := reflect.TypeOf(p)
		name := typ.Name()
		seen[name] = true

		want, ok := keyedPayloadFields[name]
		if !ok {
			t.Errorf("ir.%s has no entry in keyedPayloadFields — decide which of its fields belong in the cache key, then add it", name)
			continue
		}

		var got []string
		for i := range typ.NumField() {
			if f := typ.Field(i); f.IsExported() {
				got = append(got, f.Name)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ir.%s exported fields changed:\n got  %v\n want %v\n"+
				"A new field here changes the built filesystem but not the cache key until you hash it.\n"+
				"Decide whether it belongs in the key (see this test's doc comment), then update keyedPayloadFields.",
				name, got, want)
		}
	}

	for name := range keyedPayloadFields {
		if !seen[name] {
			t.Errorf("keyedPayloadFields pins ir.%s, but no such payload is checked — was it renamed or removed?", name)
		}
	}
}
