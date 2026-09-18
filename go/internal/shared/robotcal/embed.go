package robotcal

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// profileFS holds the profiles WendyOS ships. One per model, written once,
// reused by everyone who owns that robot — the same shape as the embedded docs
// and skills, and for the same reason: a robot fact that lives in one app's
// source is a fact no other app can ask for.
//
// Unlike assets/docs these are hand-maintained here and are not synced from a
// sibling repo, so editing them in place is the right thing to do.
//
//go:embed profiles/*.yaml
var profileFS embed.FS

const profileDir = "profiles"

// LoadProfile returns the embedded profile for a robot kind, validated.
func LoadProfile(kind string) (*Profile, error) {
	if kind == "" {
		return nil, fmt.Errorf("no robot profile selected; pass --profile with one of: %s", strings.Join(ProfileKinds(), ", "))
	}
	data, err := profileFS.ReadFile(path.Join(profileDir, kind+".yaml"))
	if err != nil {
		return nil, fmt.Errorf("no robot profile %q; WendyOS ships %s", kind, strings.Join(ProfileKinds(), ", "))
	}
	p, err := ParseProfile(data)
	if err != nil {
		return nil, fmt.Errorf("embedded profile %q: %w", kind, err)
	}
	return p, nil
}

// ProfileKinds lists the embedded profiles, sorted.
func ProfileKinds() []string {
	entries, err := fs.ReadDir(profileFS, profileDir)
	if err != nil {
		return nil
	}
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		kinds = append(kinds, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(kinds)
	return kinds
}
