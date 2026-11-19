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

// ResolveRef returns the commit of ref, a branch or a tag of the wendy-lite
// repository at url, as git clone --branch takes it: a branch wins over a
// tag of the same name, and an annotated tag gives the commit that it points
// to. tag tells whether ref is a tag.
func ResolveRef(ctx context.Context, url, ref string) (commit string, tag bool, err error) {
	heads, tags := "refs/heads/"+ref, "refs/tags/"+ref
	out, err := Git(ctx, "", "ls-remote", url, heads, tags, tags+"^{}")
	if err != nil {
		return "", false, fmt.Errorf("looking up wendy-lite %q: %w", ref, err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok {
			refs[name] = sha
		}
	}
	switch {
	case refs[heads] != "":
		return refs[heads], false, nil
	case refs[tags+"^{}"] != "":
		return refs[tags+"^{}"], true, nil
	case refs[tags] != "":
		return refs[tags], true, nil
	}
	return "", false, fmt.Errorf("wendy-lite has no branch or tag %q (check the tag or branch name)", ref)
}

// PrepareSources returns the build folder in root of commit, the commit of
// ref (a branch or a tag, see ResolveRef) in the wendy-lite repository at
// url. It reuses the folder when it is a clone of commit. Otherwise it
// clones ref without history into a staging folder in root, then renames it:
// a failed or interrupted clone never leaves a folder under the build folder
// name. When ref moved since ResolveRef, the folder is that of the cloned
// commit, which PrepareSources returns.
//
// When ref is a tag that a reused folder does not have, it fetches the tag:
// tools/configure.py then gives the version of the tag, as in a new clone.
// It sets the modification time of the folder to now, for
// PruneBuildFolders. It takes no lock: the caller makes sure that no other
// process uses root. logf reports the clone or the reuse.
func PrepareSources(ctx context.Context, root, url, ref, commit string, tag bool, logf func(format string, args ...any)) (dir, head string, err error) {
	dir = filepath.Join(root, BuildFolderName(commit))
	if reusable(ctx, dir, commit) {
		if tag {
			if _, err := Git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/tags/"+ref); err != nil {
				if _, err := Git(ctx, dir, "fetch", "--quiet", "--depth", "1", "origin", "tag", ref); err != nil {
					return "", "", fmt.Errorf("fetching the tag %s of wendy-lite: %w", ref, err)
				}
			}
		}
		logf("Reusing the build folder %s", dir)
		return dir, commit, touch(dir)
	}
	if _, err := os.Lstat(dir); err == nil {
		logf("The build folder %s is not a clone of wendy-lite %s: it is cloned again", dir, ShortVersion(commit))
		if err := os.RemoveAll(dir); err != nil {
			return "", "", fmt.Errorf("removing the build folder: %w", err)
		}
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	staging, err := os.MkdirTemp(root, cloneStagingPrefix)
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(staging)
		}
	}()
	logf("Cloning wendy-lite %s (%s) from %s into %s...", ref, ShortVersion(commit), url, dir)
	// --branch takes a tag or a branch.
	if _, err := Git(ctx, "", "clone", "--quiet", "--depth", "1", "--branch", ref, url, staging); err != nil {
		return "", "", fmt.Errorf("cloning wendy-lite %q (check the tag or branch name): %w", ref, err)
	}
	out, err := Git(ctx, staging, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(out)
	if !strings.EqualFold(head, commit) {
		logf("wendy-lite %s moved to %s during the clone", ref, ShortVersion(head))
		dir = filepath.Join(root, BuildFolderName(head))
		if err := os.RemoveAll(dir); err != nil {
			return "", "", fmt.Errorf("removing the build folder: %w", err)
		}
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", "", err
	}
	return dir, head, touch(dir)
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
		m := versionTag.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		version := make([]uint64, 3)
		ok := true
		for i := range version {
			n, err := strconv.ParseUint(m[i+1], 10, 64)
			if err != nil {
				ok = false
				break
			}
			version[i] = n
		}
		if ok && (best == nil || newerVersion(version, best)) {
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
