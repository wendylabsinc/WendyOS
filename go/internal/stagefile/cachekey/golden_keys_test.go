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
				"sha256:af5065361d0fbbce9bde07caaa4e5b8dc4abd129d4b2e4198b5da3135da97e29",
				"sha256:0806cb7cf175b910d3733d8e742c49a4a55016ed1a27e55ded858652cccbffa3",
				"sha256:e10a7f44181cced429e4074448ab357b40da887e28192372140e1df7fb366577",
				"sha256:af5065361d0fbbce9bde07caaa4e5b8dc4abd129d4b2e4198b5da3135da97e29",
				"sha256:525549c50c0b2be4880210488a402871c4304f2cc7612af0da4cae7040b3038c",
				"sha256:d3e106c5ad74ba127b59a645eb527e2068ab2805857f8170091095ce423ce5db",
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
				"sha256:f14948db391fcdca5457255da7bc9897eaab4735cf836091016bee120d5b7dfd",
				"sha256:e11cefc5aad3445de1d11cc2775c4dd63cf41d4293fe4c75f60bc119634744b0",
				"sha256:f33cd53c4a9c8d74c46dfa68a3134ef7989187aa7587fcc013256d8532aa51bd",
				"sha256:2b9ec32b97be9f52d128dc676795b2fe7e47b2ecd06766e3320adeb47a1f20aa",
				"sha256:43bda82d75a3ff1a5e2cc3a6ce0b848baf643571ff01360b4abd4affc5937d6e",
				"sha256:1243a24800c4125e5dac389edd627c041e914da4284fa3ebd67231c6336779ea",
				"sha256:f14948db391fcdca5457255da7bc9897eaab4735cf836091016bee120d5b7dfd",
				"sha256:8fb7b638047b032559ab0684813075444aee9ae4a80ea2f2e83a1a76564576f3",
				"sha256:2a0a243246b5aa580a2405193ea6f6e063920fccc8eb3eddf8f9365a323232fa",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
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
			if len(g.Nodes) != len(tc.want) {
				t.Fatalf("fixture lowered to %d nodes, corpus pins %d — update both together", len(g.Nodes), len(tc.want))
			}
			for i := range g.Nodes {
				got, err := Key(g, i, tc.in)
				if err != nil {
					t.Fatalf("Key(node %d): %v", i, err)
				}
				if got != tc.want[i] {
					t.Errorf("node %d (%s) key drift:\n got  %s\n want %s", i, g.Nodes[i].Kind, got, tc.want[i])
				}
			}
		})
	}
}
