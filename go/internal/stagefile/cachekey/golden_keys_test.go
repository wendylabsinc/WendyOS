package cachekey

import (
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// TestGoldenKeys pins the key of every node in every shipped fixture
// Stagefile. These strings are a wire format, not an implementation detail:
// changing one invalidates that node for every cache tier, in every org,
// permanently. A diff here is only ever correct alongside a bump to
// ir.Recipe.Version (scoped) or keyFormatVersion (global), and both belong
// in their own reviewed commit.
//
// Between them the fixtures must reach all five recipes and both copy
// flavors. example.stagefile.yaml covers image, apt, pip, and copy;
// npmbuild.stagefile.yaml covers npm, apk, and build. A recipe with no
// frozen key is a recipe whose params can gain or lose a field with no test
// noticing — which is precisely how npm's package.json digest went missing.
func TestGoldenKeys(t *testing.T) {
	cases := []struct {
		fixture string
		in      Inputs
		want    []string
	}{
		{
			fixture: "example.stagefile.yaml",
			in: Inputs{
				Images: map[string]string{"python:3.12-slim": "sha256:abc123"},
				Files: map[string]string{
					"requirements.txt": "sha256:fixedreqs",
					"app.py":           "sha256:fixedapp",
				},
				Platform: "linux/arm64",
			},
			want: []string{
				"sha256:d66b2224c7741edfcf37c844040c2d93ebeb503b492bfdbb3604f70ed7ddfc0c",
				"sha256:69580d9c8f04a6c1686d2c6f10d947360991511276754e9db3d8ea5ec2484dd4",
				"sha256:38f52f5e45682e7b3cd569679d2d07ead390bf12e162b99f26a3bfc412cfc6d1",
				"sha256:d66b2224c7741edfcf37c844040c2d93ebeb503b492bfdbb3604f70ed7ddfc0c",
				"sha256:eb45219ba8553f883e445438af1a09035bfc99ee188d19ca7671050165dda26f",
				"sha256:1c23146276118cba692e45723d949ef6cb28b950fd21846d80ec1d779616c8b7",
				"sha256:d66b2224c7741edfcf37c844040c2d93ebeb503b492bfdbb3604f70ed7ddfc0c",
				"sha256:2ea558fb9e467674eb38d7291a1ef4876716790bfbe6fed12e21aa4edd98d567",
				"sha256:1eb41b246411d79a01ebf2444eff83f87e5d00f60ba617c4c1cd8edb0345da6a",
			},
		},
		{
			fixture: "npmbuild.stagefile.yaml",
			in: Inputs{
				Images: map[string]string{
					"node:20-slim":  "sha256:node20",
					"rust:1-alpine": "sha256:rust1",
				},
				Files: map[string]string{
					"package.json":      "sha256:fixedpkgjson",
					"package-lock.json": "sha256:fixedpkglock",
					"src":               "sha256:fixedsrc",
					"tsconfig.json":     "sha256:fixedtsconfig",
				},
				Platform: "linux/arm64",
			},
			want: []string{
				"sha256:4a65491fc80e664288abe94aadb7c7841573124c4915e91a096c27373b13313a",
				"sha256:7379f0e72f6fad183a885c839422f196511b2973151290cbf1fe9d81d2d162fd",
				"sha256:edf6ddfd6342e760b9e829964e99e09e202981a661a4bfb0b5dd74733c2cdbf6",
				"sha256:e7b3e39854c61c26044f8b338d52549371b73951cdc80a7a998d7e52d8494b52",
				"sha256:6ddcd21703dc811eb7e89e13024237e2b7d9ff06352181d460017d8740be1204",
				"sha256:31a9903faef66c2ac2c307bd815aad66ad8e397c094f7f310ed34741dacdeeb2",
				"sha256:4a65491fc80e664288abe94aadb7c7841573124c4915e91a096c27373b13313a",
				"sha256:203af77f22d456d1ab53bfb9617661249a02092f6bf9eb64e252e28d93dff73a",
				"sha256:1abb97e78815e7917aa5affba356b63398bff6bdbe5d75665af0e6b4494114cf",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			update := os.Getenv("UPDATE_STAGEFILE_KEYS") != ""
			data, err := os.ReadFile("../testdata/" + tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			f, err := spec.Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			// Lowered at the same platform the Inputs name: the graph now
			// carries the per-stage resolution of it, so lowering at one
			// platform and keying at another would freeze a combination no
			// real build can produce.
			g, err := ir.Lower(f, ir.Options{Platform: tc.in.Platform})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			if !update && len(g.Nodes) != len(tc.want) {
				t.Fatalf("fixture lowered to %d nodes, corpus pins %d — update both together", len(g.Nodes), len(tc.want))
			}
			var harvested []string
			for i := range g.Nodes {
				got, err := Key(g, i, tc.in)
				if err != nil {
					t.Fatalf("Key(node %d): %v", i, err)
				}
				harvested = append(harvested, got)
				if !update && got != tc.want[i] {
					t.Errorf("node %d (%s) key drift:\n got  %s\n want %s", i, g.Nodes[i].Kind, got, tc.want[i])
				}
			}
			if update {
				t.Logf("%q", harvested)
			}
		})
	}
}
