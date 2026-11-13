package jobops

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func validateArtifactDigests(files, selected []string) error {
	if len(selected) > 16 {
		return fmt.Errorf("select at most 16 artifact digests")
	}
	allowed := map[string]bool{}
	for _, name := range files {
		allowed[name] = true
	}
	seen := map[string]bool{}
	budget := map[string]string{}
	for _, name := range selected {
		budget[name] = strings.Repeat("0", 64)
		if !allowed[name] || seen[name] {
			return fmt.Errorf("artifact_digests must uniquely select collected files within a 4096-byte reply budget")
		}
		seen[name] = true
	}
	encoded, _ := json.Marshal(budget)
	if len(encoded) > 4096 {
		return fmt.Errorf("selected artifact digests exceed the 4096-byte reply budget")
	}
	return nil
}

// selectedArtifactDigests exposes only explicitly requested verified file
// hashes. Their manifest remains available locally for larger selections.
func selectedArtifactDigests(destination string, artifacts map[string]any, selected []string) (map[string]string, error) {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, err := readRootRegular(root, ".wendy-artifacts-manifest.json", maxProjectedReport)
	if err != nil {
		return nil, err
	}
	if digest(raw) != artifacts["manifest_sha256"] {
		return nil, fmt.Errorf("artifact manifest changed before digest selection")
	}
	var manifest struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("invalid verified artifact manifest")
	}
	hashes := map[string]string{}
	for _, row := range manifest.Files {
		hashes[row.Path] = row.SHA256
	}
	out := map[string]string{}
	for _, name := range selected {
		wanted := hashes[name]
		decoded, err := hex.DecodeString(wanted)
		if err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("verified manifest lacks requested digest: %s", name)
		}
		// Recheck selected local bytes, not only the earlier collection claim.
		data, err := readRootRegular(root, name, 16*1024*1024)
		if err != nil || digest(data) != wanted {
			return nil, fmt.Errorf("artifact changed before digest selection: %s", name)
		}
		out[name] = wanted
	}
	return out, nil
}
