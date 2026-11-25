package patrolkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Catalog is trusted local, owner-private state. Offline kits are not portable
// authority roots: copying a kit without this separately retained pin is refused.
type Catalog struct {
	Path string
	lock *os.File
}

func Open(path string) (*Catalog, error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(absolute, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(absolute)
	if e != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("offline catalog requires a private real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("offline catalog owner differs")
	}
	f, e := os.OpenFile(filepath.Join(absolute, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("offline catalog busy")
	}
	c := &Catalog{Path: absolute, lock: f}
	for _, name := range []string{"kits", "pins", "claims", "authority"} {
		p := filepath.Join(absolute, name)
		if e = os.MkdirAll(p, 0700); e != nil {
			c.Close()
			return nil, e
		}
		s, e := os.Lstat(p)
		if e != nil || !s.IsDir() || s.Mode().Perm()&0077 != 0 {
			c.Close()
			return nil, errors.New("invalid catalog directory")
		}
	}
	return c, nil
}
func (c *Catalog) Close() error {
	if c.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
	e := c.lock.Close()
	c.lock = nil
	return e
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func exclusive(path string, raw []byte, mode os.FileMode) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if e != nil {
		return e
	}
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = syncDir(filepath.Dir(path))
	}
	return e
}
func privateJSON(path string, out any) error {
	raw, e := ReadRegular(path, 1<<20)
	if e != nil {
		return e
	}
	return authority.DecodeStrictLimit(raw, out, 1<<20)
}
func (c *Catalog) Register(profile Profile, bundle worldpackage.Bundle, approval Approval, executor, viewer []byte) (Entry, error) {
	var zero Entry
	if !token.MatchString(profile.ID) || profile.Runtime.Validate() != nil || len(executor) < 1 || len(executor) > MaxRuntimeBytes || len(viewer) < 1 || len(viewer) > 1<<20 || Digest(executor) != profile.Runtime.ExecutorSHA256 || Digest(viewer) != profile.Runtime.ViewerSHA256 || bundle.Manifest.SHA256 != profile.PackageSHA256 || bundle.Manifest.Site != "studio-01" || bundle.Manifest.FrameRevision != profile.Frame.Revision {
		return zero, errors.New("invalid prepared runtime/package closure")
	}
	entries, e := os.ReadDir(filepath.Join(c.Path, "pins"))
	if e != nil {
		return zero, e
	}
	if len(entries) >= MaxEntries {
		return zero, errors.New("offline catalog is full")
	}
	entry := Entry{Profile: profile, RootPublicKey: approval.RootPublicKey}
	if _, _, e = VerifyApproval(approval, entry, time.Now()); e != nil {
		return zero, e
	}
	stage, e := os.MkdirTemp(c.Path, ".prepare-")
	if e != nil {
		return zero, e
	}
	defer os.RemoveAll(stage)
	if e = worldpackage.Install(bundle, filepath.Join(stage, "world")); e != nil {
		return zero, e
	}
	if e = os.Mkdir(filepath.Join(stage, "runtime"), 0700); e != nil {
		return zero, e
	}
	for name, data := range map[string][]byte{"runtime/world-patrol": executor, "runtime/patrol_viewer.py": viewer} {
		if e = exclusive(filepath.Join(stage, name), data, 0500); e != nil {
			return zero, e
		}
	}
	raw, e := json.Marshal(approval)
	if e != nil {
		return zero, e
	}
	if e = exclusive(filepath.Join(stage, "approval.json"), raw, 0400); e != nil {
		return zero, e
	}
	manifest := Manifest{Schema: Schema, ID: profile.ID, Files: []worldpackage.File{}}
	var total int64
	e = filepath.WalkDir(stage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(stage, path)
		raw, e := ReadRegular(path, MaxRuntimeBytes)
		if e != nil {
			return e
		}
		total += int64(len(raw))
		if total > MaxKitBytes {
			return errors.New("kit closure exceeds 128 MiB")
		}
		manifest.Files = append(manifest.Files, worldpackage.File{Path: filepath.ToSlash(rel), SHA256: Digest(raw), Size: int64(len(raw))})
		return nil
	})
	if e != nil {
		return zero, e
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	raw, _ = json.Marshal(manifest)
	entry.KitSHA256 = Digest(raw)
	if e = exclusive(filepath.Join(stage, "kit.json"), raw, 0400); e != nil {
		return zero, e
	}
	dest := filepath.Join(c.Path, "kits", profile.ID)
	if _, e = os.Lstat(dest); !os.IsNotExist(e) {
		return zero, errors.New("kit identifier already exists")
	}
	if e = os.Rename(stage, dest); e != nil {
		return zero, e
	}
	if e = syncDir(filepath.Join(c.Path, "kits")); e != nil {
		return zero, e
	}
	raw, _ = json.Marshal(entry)
	if e = exclusive(filepath.Join(c.Path, "pins", profile.ID+".json"), raw, 0600); e != nil {
		return zero, e
	}
	// A published pin is checked against the fully installed closure before success.
	if _, e = c.Verify(profile.ID, time.Now()); e != nil {
		return zero, e
	}
	return entry, nil
}

// checkpointClock also remembers failed/expired inspections. Rolling the host
// wall clock back cannot make an approval that was seen expired usable again.
func (c *Catalog) checkpointClock(now time.Time) error {
	journal, e := authority.OpenReplay(filepath.Join(c.Path, "authority", "catalog-clock.json"), 1)
	if e != nil {
		return e
	}
	defer journal.Close()
	return journal.Checkpoint(1, Digest([]byte(Schema+"/local-catalog-clock")), now)
}
func (c *Catalog) Verify(id string, now time.Time) (Verified, error) {
	var out Verified
	if !token.MatchString(id) {
		return out, errors.New("invalid kit identifier")
	}
	if e := c.checkpointClock(now); e != nil {
		return out, e
	}
	if e := privateJSON(filepath.Join(c.Path, "pins", id+".json"), &out.Entry); e != nil {
		return out, e
	}
	if out.Entry.Profile.ID != id {
		return out, errors.New("catalog pin identity differs")
	}
	dir := filepath.Join(c.Path, "kits", id)
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() {
		return out, errors.New("kit installation unavailable")
	}
	raw, e := ReadRegular(filepath.Join(dir, "kit.json"), 128<<10)
	if e != nil {
		return out, e
	}
	if Digest(raw) != out.Entry.KitSHA256 {
		return out, errors.New("kit manifest differs from local approval pin")
	}
	var manifest Manifest
	if e = authority.DecodeStrictLimit(raw, &manifest, 128<<10); e != nil {
		return out, e
	}
	if manifest.Schema != Schema || manifest.ID != id || len(manifest.Files) < 5 || len(manifest.Files) > worldpackage.MaxFiles+8 {
		return out, errors.New("invalid kit closure")
	}
	out.Files = map[string][]byte{}
	var total int64
	for _, f := range manifest.Files {
		if worldpackage.SafePath(f.Path) != nil || f.Size < 1 || f.Size > MaxRuntimeBytes || !validDigest(f.SHA256) {
			return out, errors.New("invalid kit file")
		}
		if _, ok := out.Files[f.Path]; ok {
			return out, errors.New("duplicate kit file")
		}
		// Reject symlink components before opening. The catalog is trusted local storage;
		// a verified private copy is made for activation rather than running in place.
		cursor := dir
		parts := splitPath(f.Path)
		for i, part := range parts {
			cursor = filepath.Join(cursor, part)
			s, e := os.Lstat(cursor)
			if e != nil || s.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !s.IsDir()) {
				return out, errors.New("kit file path changed")
			}
		}
		data, e := ReadRegular(filepath.Join(dir, filepath.FromSlash(f.Path)), f.Size)
		if e != nil {
			return out, e
		}
		if int64(len(data)) != f.Size || Digest(data) != f.SHA256 {
			return out, fmt.Errorf("kit file changed: %s", f.Path)
		}
		total += f.Size
		if total > MaxKitBytes {
			return out, errors.New("kit exceeds total bound")
		}
		out.Files[f.Path] = data
	}
	count := 0
	e = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "kit.json" {
			return nil
		}
		count++
		if _, ok := out.Files[filepath.ToSlash(rel)]; !ok {
			return errors.New("undeclared file in kit")
		}
		return nil
	})
	if e != nil || count != len(out.Files) {
		return out, errors.New("kit closure differs")
	}
	if e = authority.DecodeStrictLimit(out.Files["approval.json"], &out.Approval, authority.MaxEnvelopeBytes*3); e != nil {
		return out, e
	}
	blobs := map[string][]byte{}
	for name, b := range out.Files {
		if len(name) > 6 && name[:6] == "world/" && name != "world/world.json" {
			blobs[name[6:]] = b
		}
	}
	out.Bundle, e = worldpackage.Verify(out.Files["world/world.json"], blobs)
	if e != nil {
		return out, e
	}
	if out.Bundle.Manifest.SHA256 != out.Entry.Profile.PackageSHA256 || out.Bundle.Manifest.Site != "studio-01" || out.Bundle.Manifest.FrameRevision != out.Entry.Profile.Frame.Revision || Digest(out.Files["runtime/world-patrol"]) != out.Entry.Profile.Runtime.ExecutorSHA256 || Digest(out.Files["runtime/patrol_viewer.py"]) != out.Entry.Profile.Runtime.ViewerSHA256 {
		return out, errors.New("prepared closure binding differs")
	}
	out.Spec, _, e = VerifyApproval(out.Approval, out.Entry, now)
	if e != nil {
		return out, e
	}
	replay, e := authority.OpenReplay(filepath.Join(c.Path, "authority", id+".json"), 128)
	if e != nil {
		return out, e
	}
	defer replay.Close()
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: out.Entry.RootPublicKey, Tenant: "simulation", Site: "studio-01", Domain: "simulation", Audience: Audience, Now: func() time.Time { return now }, MaxRevocationAge: 25 * time.Hour, MaxGrantLifetime: 25 * time.Hour, Replay: replay})
	if e == nil {
		e = v.UpdateRevocations(out.Approval.Revocations)
	}
	if e == nil {
		_, e = v.VerifyInstruction(out.Approval.Instruction, "mission")
	}
	return out, e
}
func splitPath(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
