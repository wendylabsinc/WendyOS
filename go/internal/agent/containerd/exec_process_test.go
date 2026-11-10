package containerd

import (
	"encoding/json"
	"testing"

	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestAppExecProcessResetsOOMProtectionWithoutChangingSpec(t *testing.T) {
	protected := -998
	for name, score := range map[string]*int{"legacy": nil, "protected": &protected} {
		t.Run(name, func(t *testing.T) {
			original := &runtimespec.Process{
				Args: []string{"original"}, Env: []string{"ORIGINAL=1"},
				Cwd: "/work", OOMScoreAdj: score,
			}
			process := appExecProcess(original)
			data, err := json.Marshal(process)
			if err != nil {
				t.Fatal(err)
			}
			var decoded runtimespec.Process
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.OOMScoreAdj == nil || *decoded.OOMScoreAdj != 0 {
				t.Fatalf("exec must serialize explicit oomScoreAdj=0: %s", data)
			}
			if process.Cwd != "/work" {
				t.Fatal("exec lost original process settings")
			}
			process.Args[0] = "changed"
			process.Env[0] = "CHANGED=1"
			*process.OOMScoreAdj = 100
			if original.Args[0] != "original" || original.Env[0] != "ORIGINAL=1" || original.OOMScoreAdj != score {
				t.Fatal("exec mutated the original process")
			}
			if score != nil && *score != -998 {
				t.Fatal("exec mutated the original OOM score")
			}
		})
	}
}
