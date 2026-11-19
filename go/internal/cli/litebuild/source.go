package litebuild

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
)

// The build folders of the Wendy Lite build from source are in a root
// folder of the CLI cache, one folder per wendy-lite commit, named
// BuildFolderName(commit). A folder is a git clone of that commit, and the
// ESP-IDF build of the last configuration of that commit. The next build of
// the same commit reuses it.

// MaxBuildFolders is the number of build folders that PruneBuildFolders
// keeps: the folder of the current build and the most recently used other
// folders.
const MaxBuildFolders = 2

// buildFolderNameLength is the length of a build folder name: hex digits of
// the commit. 12 digits identify a commit and keep the ESP-IDF paths short,
// which matters on Windows.
const buildFolderNameLength = 12

// cloneStagingPrefix starts the name of the folder that PrepareSources
// clones into, before it renames it to the build folder name.
const cloneStagingPrefix = ".clone-"

// BuildFolderName returns the name of the build folder of commit, a full
// commit SHA: its first 12 hex digits.
func BuildFolderName(commit string) string {
	commit = strings.ToLower(commit)
	if len(commit) > buildFolderNameLength {
		return commit[:buildFolderNameLength]
	}
	return commit
}

// isBuildFolderName tells whether name is a BuildFolderName.
func isBuildFolderName(name string) bool {
	return len(name) == buildFolderNameLength && strings.Trim(name, "0123456789abcdef") == ""
}

// NightlyTag is the wendy-lite tag of the nightly build. The tag moves: the
// build checks out its commit.
const NightlyTag = "nightly"

var (
	// fullCommit matches a full commit hash.
	fullCommit = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	// abbreviatedCommit matches what can be an abbreviated commit hash.
	abbreviatedCommit = regexp.MustCompile(`^[0-9a-fA-F]{7,39}$`)
)

// Source is a Wendy Lite version to build, and its wendy-lite commit (see
// ResolveSource).
type Source struct {
	// Name is the version as it was asked for, for the messages: "0.15.0",
	// "nightly", a branch name or a commit hash.
	Name string
	// Version is the Wendy Lite version, as tools/configurator/wendy_version.py
	// of wendy-lite gives it and a device reports it: "X.Y.Z" for the release
	// tag vX.Y.Z, else the full commit hash.
	Version string
	// Commit is the full hash of the commit.
	Commit string
}

// Ref returns the git ref of the version: the release tag vX.Y.Z, or the
// commit hash.
func (s Source) Ref() string {
	return espidftoolchain.WendyLiteGitRef(s.Version)
}

// releaseTag returns the release tag vX.Y.Z of the version, or "" when the
// version is a commit hash.
func (s Source) releaseTag() string {
	if ref := s.Ref(); ref != s.Version {
		return ref
	}
	return ""
}

// String returns the version for the messages: the release "X.Y.Z", or the
// name and the short commit hash, for example "nightly (8032f6d)".
func (s Source) String() string {
	switch {
	case s.releaseTag() != "":
		return s.Name
	case strings.EqualFold(s.Name, s.Commit):
		return ShortVersion(s.Commit)
	}
	return fmt.Sprintf("%s (%s)", s.Name, ShortVersion(s.Commit))
}

// ResolveSource returns the Wendy Lite version to build from the wendy-lite
// repository at url:
//
//   - with nightly, the commit of the nightly tag (NightlyTag);
//   - with version "X.Y.Z" or "vX.Y.Z", the release tag vX.Y.Z;
//   - with version a full commit hash, that commit, which it does not look
//     up: git ls-remote lists only branches and tags;
//   - with another version, the commit of the branch or the tag of that name
//     (see resolveRef);
//   - else the latest release: the highest vX.Y.Z tag.
//
// nightly wins over version.
func ResolveSource(ctx context.Context, url string, nightly bool, version string) (Source, error) {
	switch {
	case nightly:
		commit, err := resolveTag(ctx, url, NightlyTag)
		if err != nil {
			return Source{}, err
		}
		if commit == "" {
			return Source{}, fmt.Errorf("wendy-lite has no %s tag", NightlyTag)
		}
		return Source{Name: NightlyTag, Version: commit, Commit: commit}, nil
	case version == "":
		return latestRelease(ctx, url)
	case fullCommit.MatchString(version):
		commit := strings.ToLower(version)
		return Source{Name: commit, Version: commit, Commit: commit}, nil
	}
	if tag := "v" + strings.TrimPrefix(version, "v"); versionTag.MatchString(tag) {
		commit, err := resolveTag(ctx, url, tag)
		if err != nil {
			return Source{}, err
		}
		release := tag[1:]
		if commit == "" {
			return Source{}, fmt.Errorf("Wendy Lite %s does not exist: wendy-lite has no tag %s", release, tag)
		}
		return Source{Name: release, Version: release, Commit: commit}, nil
	}
	commit, err := resolveRef(ctx, url, version)
	if err != nil {
		return Source{}, err
	}
	return Source{Name: version, Version: commit, Commit: commit}, nil
}

// latestRelease returns the latest release of the wendy-lite repository at
// url: the highest vX.Y.Z tag, compared as numbers.
func latestRelease(ctx context.Context, url string) (Source, error) {
	refs, err := lsRemote(ctx, "--tags", url)
	if err != nil {
		return Source{}, fmt.Errorf("looking up the wendy-lite releases: %w", err)
	}
	var best []uint64
	var bestTag string
	for name := range refs {
		tag, ok := strings.CutPrefix(name, "refs/tags/")
		if !ok {
			continue
		}
		version, ok := releaseNumbers(tag)
		// The text of the tags breaks the ties, for the same result each time.
		if ok && (best == nil || newerVersion(version, best) || !newerVersion(best, version) && tag < bestTag) {
			best, bestTag = version, tag
		}
	}
	if best == nil {
		return Source{}, errors.New("wendy-lite has no release tag vX.Y.Z")
	}
	release := bestTag[1:]
	return Source{Name: release, Version: release, Commit: peeled(refs, "refs/tags/"+bestTag)}, nil
}

// resolveTag returns the commit of tag in the wendy-lite repository at url,
// or "" when the repository has no such tag. An annotated tag gives the
// commit that it points to.
func resolveTag(ctx context.Context, url, tag string) (string, error) {
	name := "refs/tags/" + tag
	refs, err := lsRemote(ctx, url, name, name+"^{}")
	if err != nil {
		return "", fmt.Errorf("looking up the wendy-lite tag %s: %w", tag, err)
	}
	return peeled(refs, name), nil
}

// resolveRef returns the commit of ref, a branch or a tag of the wendy-lite
// repository at url: a branch wins over a tag of the same name, as with git
// clone --branch, and an annotated tag gives the commit that it points to.
func resolveRef(ctx context.Context, url, ref string) (string, error) {
	heads, tags := "refs/heads/"+ref, "refs/tags/"+ref
	refs, err := lsRemote(ctx, url, heads, tags, tags+"^{}")
	if err != nil {
		return "", fmt.Errorf("looking up wendy-lite %q: %w", ref, err)
	}
	if commit := refs[heads]; commit != "" {
		return commit, nil
	}
	if commit := peeled(refs, tags); commit != "" {
		return commit, nil
	}
	if abbreviatedCommit.MatchString(ref) {
		return "", fmt.Errorf("wendy-lite has no branch or tag %q; for a commit, give its full hash (40 hexadecimal digits)", ref)
	}
	return "", fmt.Errorf("wendy-lite has no branch or tag %q (check the tag or branch name)", ref)
}

// lsRemote runs git ls-remote with args and returns the refs that it lists:
// the full ref name, for example "refs/tags/v1.2.3" or "refs/tags/v1.2.3^{}"
// (the commit of an annotated tag), and its object hash.
func lsRemote(ctx context.Context, args ...string) (map[string]string, error) {
	out, err := Git(ctx, "", append([]string{"ls-remote"}, args...)...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok {
			refs[name] = sha
		}
	}
	return refs, nil
}

// peeled returns the commit of the ref name in refs (see lsRemote): for an
// annotated tag, the commit that it points to. It returns "" when refs has
// no such ref.
func peeled(refs map[string]string, name string) string {
	if commit := refs[name+"^{}"]; commit != "" {
		return commit
	}
	return refs[name]
}

// PrepareSources returns the build folder in root of src, a version of the
// wendy-lite repository at url (see ResolveSource). It reuses the folder
// when it is a clone of src.Commit. Otherwise it fetches that one commit,
// without history, into a staging folder in root and checks it out, then it
// renames the folder: a failed or interrupted clone never leaves a folder
// under the build folder name. It checks out the commit, never a branch or a
// tag, which can move.
//
// For a release, the folder also has the tag vX.Y.Z, also when it is
// reused: tools/configure.py then gives the version X.Y.Z, as in a clone of
// the tag. It sets the modification time of the folder to now, for
// PruneBuildFolders. It takes no lock: the caller makes sure that no other
// process uses root. logf reports the clone or the reuse.
func PrepareSources(ctx context.Context, root, url string, src Source, logf func(format string, args ...any)) (dir string, err error) {
	dir = filepath.Join(root, BuildFolderName(src.Commit))
	tag := src.releaseTag()
	if reusable(ctx, dir, src.Commit) {
		if tag != "" {
			if _, err := Git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag); err != nil {
				if _, err := Git(ctx, dir, "fetch", "--quiet", "--depth", "1", "origin", "tag", tag); err != nil {
					return "", fmt.Errorf("fetching the tag %s of wendy-lite: %w", tag, err)
				}
			}
		}
		logf("Reusing the build folder %s", dir)
		return dir, touch(dir)
	}
	if _, err := os.Lstat(dir); err == nil {
		logf("The build folder %s is not a clone of wendy-lite %s: it is cloned again", dir, src)
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("removing the build folder: %w", err)
		}
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(root, cloneStagingPrefix)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(staging)
		}
	}()
	logf("Cloning wendy-lite %s from %s into %s...", src, url, dir)
	if err := cloneCommit(ctx, staging, url, src.Commit, tag); err != nil {
		return "", fmt.Errorf("cloning wendy-lite %s: %w", src, err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", err
	}
	return dir, touch(dir)
}

// cloneCommit makes the empty folder dir a checkout of commit of the
// wendy-lite repository at url, without history. With tag, it also fetches
// that tag. The remote is origin, as in a git clone.
func cloneCommit(ctx context.Context, dir, url, commit, tag string) error {
	fetch := []string{"fetch", "--quiet", "--no-tags", "--depth", "1", "origin", commit}
	if tag != "" {
		fetch = append(fetch, "refs/tags/"+tag+":refs/tags/"+tag)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", url},
		fetch,
		{"checkout", "--quiet", "--detach", commit},
	} {
		if _, err := Git(ctx, dir, args...); err != nil {
			return err
		}
	}
	return nil
}

// reusable tells whether dir is a git clone whose HEAD is commit.
func reusable(ctx context.Context, dir, commit string) bool {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false
	}
	out, err := Git(ctx, dir, "rev-parse", "HEAD")
	return err == nil && strings.EqualFold(strings.TrimSpace(out), commit)
}

// touch sets the modification time of dir to now: the time of its last use.
func touch(dir string) error {
	now := time.Now()
	return os.Chtimes(dir, now, now)
}

// PruneBuildFolders removes the build folders in root, except the folder of
// commit (it may not exist yet) and the MaxBuildFolders-1 other folders that
// were used last (modification time, see PrepareSources). It also removes
// the staging folders of interrupted clones. It leaves the other files and
// folders of root. A folder that cannot be removed gives a warning (warnf).
// It takes no lock: the caller makes sure that no other process uses root.
// logf reports each removal.
func PruneBuildFolders(root, commit string, logf, warnf func(format string, args ...any)) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			warnf("The old build folders are not removed: %v", err)
		}
		return
	}
	type folder struct {
		path string
		used time.Time
	}
	var others []folder
	keep := BuildFolderName(commit)
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, name)
		if strings.HasPrefix(name, cloneStagingPrefix) {
			if err := os.RemoveAll(path); err != nil {
				warnf("The folder %s of an interrupted clone is not removed: %v", path, err)
			}
			continue
		}
		if name == keep || !isBuildFolderName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		others = append(others, folder{path, info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool { return others[i].used.After(others[j].used) })
	for i, f := range others {
		if i < MaxBuildFolders-1 {
			continue
		}
		logf("Removing the old build folder %s...", f.path)
		if err := os.RemoveAll(f.path); err != nil {
			warnf("The old build folder %s is not removed: %v", f.path, err)
		}
	}
}

// versionTag matches a release tag vX.Y.Z.
var versionTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// SourceVersion returns the Wendy Lite version of the git checkout in dir,
// as tools/configurator/wendy_version.py of wendy-lite gives it for
// CONFIG_WENDY_VERSION when the tracked files have no changes: X.Y.Z when
// HEAD has the tag vX.Y.Z (the highest version when it has several), else
// the full commit hash of HEAD.
func SourceVersion(ctx context.Context, dir string) (string, error) {
	out, err := Git(ctx, dir, "tag", "--points-at", "HEAD")
	if err != nil {
		return "", err
	}
	var best []uint64
	for _, tag := range strings.Fields(out) {
		if version, ok := releaseNumbers(tag); ok && (best == nil || newerVersion(version, best)) {
			best = version
		}
	}
	if best != nil {
		return fmt.Sprintf("%d.%d.%d", best[0], best[1], best[2]), nil
	}
	out, err = Git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// releaseNumbers returns the numbers X, Y and Z of the release tag vX.Y.Z,
// or false when tag is not a release tag.
func releaseNumbers(tag string) ([]uint64, bool) {
	m := versionTag.FindStringSubmatch(tag)
	if m == nil {
		return nil, false
	}
	version := make([]uint64, 3)
	for i := range version {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return nil, false
		}
		version[i] = n
	}
	return version, true
}

// newerVersion tells whether version a is higher than version b.
func newerVersion(a, b []uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// ConfigureReason tells why the wendy-lite checkout in dir needs
// tools/configure.py: its sdkconfig is missing, or it is not for the build
// profile (compared as YAML maps) or for the version (SourceVersion). It
// returns "" when the sdkconfig is for both. configure.py writes them to
// sdkconfig.defaults.wendy, and idf.py set-target copies them to sdkconfig:
// a set-target that failed leaves no sdkconfig.
func ConfigureReason(dir, profile, version string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "sdkconfig"))
	if errors.Is(err, fs.ErrNotExist) {
		return "the build folder is not configured yet", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading sdkconfig: %w", err)
	}
	values := kconfigStrings(string(data))
	if !sameProfile(storedProfile(values), profile) {
		return "the answers are not those of the last configuration", nil
	}
	if configured := values[VersionOption]; configured != version {
		return fmt.Sprintf("the build folder is configured for Wendy Lite %q, not %q", ShortVersion(configured), ShortVersion(version)), nil
	}
	return "", nil
}
