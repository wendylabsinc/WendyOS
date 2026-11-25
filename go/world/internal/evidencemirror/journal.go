// Package evidencemirror stores immutable operator transfer intent and last-known
// progress. It grants no authority and does not claim capture ownership.
package evidencemirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
)

const MaxBytes = 64 << 20
const journalLimit = 32 << 10

var ErrInvalid = errors.New("invalid evidence mirror plan or progress")
var ErrChanged = errors.New("mirror plan scope or immutable metadata changed")
var ErrExpired = errors.New("original source evidence deadline expired")
var ErrUnavailable = errors.New("mirror journal unavailable; reopen after uncertain write")
var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type Endpoint struct {
	Tenant    string `json:"tenant"`
	Site      string `json:"site"`
	Domain    string `json:"domain"`
	Audience  string `json:"audience"`
	URL       string `json:"url"`
	Principal string `json:"principal"`
	KeySHA256 string `json:"key_sha256"`
}
type Reference struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}
type Plan struct {
	Version           int       `json:"version"`
	ID                string    `json:"id"`
	Source            Endpoint  `json:"source"`
	Destination       Endpoint  `json:"destination"`
	Reference         Reference `json:"reference"`
	RetainUntilUnixMS int64     `json:"retain_until_unix_ms"`
	PreparedUnixMS    int64     `json:"prepared_unix_ms"`
}
type State struct {
	Plan                  Plan   `json:"plan"`
	PlanSHA256            string `json:"plan_sha256"`
	Phase                 string `json:"phase"`
	Attempts              uint64 `json:"attempts"`
	SourceResumedBytes    int64  `json:"source_resumed_bytes"`
	SourceDownloadedBytes int64  `json:"source_downloaded_bytes"`
	DestinationReceived   int64  `json:"destination_received"`
	DestinationComplete   bool   `json:"destination_complete"`
	VerifiedUnixMS        int64  `json:"verified_unix_ms"`
	LastUnixMS            int64  `json:"last_unix_ms"`
	LastError             string `json:"last_error,omitempty"`
}
type Status struct {
	State
	Availability                string `json:"availability"`
	CurrentVerification         bool   `json:"current_verification"`
	OriginalCaptureOwnerClaimed bool   `json:"original_capture_owner_claimed"`
}
type envelope struct {
	SHA256 string          `json:"sha256"`
	State  json.RawMessage `json:"state"`
}
type Journal struct {
	root   *os.Root
	lock   *os.File
	path   string
	info   os.FileInfo
	state  State
	fault  bool
	closed bool
}

var writeHook func(string) // test-only child-kill boundary
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func digest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func bounded(s string) bool { return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n") }
func (e Endpoint) Validate() error {
	u, err := url.Parse(e.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") || e.Domain != "simulation" || !bounded(e.Tenant) || !bounded(e.Site) || !bounded(e.Audience) || !bounded(e.Principal) || !digest(e.KeySHA256) || len(e.URL) > 2048 {
		return ErrInvalid
	}
	return nil
}
func (p Plan) Validate() error {
	if p.Version != 1 || !identifier.MatchString(p.ID) || p.Source.Validate() != nil || p.Destination.Validate() != nil || p.Source.Tenant != p.Destination.Tenant || p.Source.Site != p.Destination.Site || p.Source.URL == p.Destination.URL || !digest(p.Reference.SHA256) || p.Reference.Size < 1 || p.Reference.Size > MaxBytes || !packagecache.ValidContentType(p.Reference.ContentType) || p.PreparedUnixMS <= 0 || p.RetainUntilUnixMS <= p.PreparedUnixMS {
		return ErrInvalid
	}
	return nil
}
func (p Plan) Digest() string { b, _ := json.Marshal(p); return hash(b) }
func (s State) validate() error {
	if s.Plan.Validate() != nil || s.PlanSHA256 != s.Plan.Digest() || s.LastUnixMS < s.Plan.PreparedUnixMS || s.DestinationReceived < 0 || s.DestinationReceived > s.Plan.Reference.Size || s.SourceResumedBytes < 0 || s.SourceResumedBytes > s.Plan.Reference.Size || s.SourceDownloadedBytes < 0 || s.SourceDownloadedBytes > MaxBytes+1 || s.VerifiedUnixMS < 0 || s.VerifiedUnixMS > s.LastUnixMS || s.VerifiedUnixMS >= s.Plan.RetainUntilUnixMS || len(s.LastError) > 128 {
		return ErrInvalid
	}
	switch s.Phase {
	case "prepared", "downloading", "uploading", "verifying", "verified", "blocked", "expired":
	default:
		return ErrInvalid
	}
	if s.DestinationComplete && s.DestinationReceived != s.Plan.Reference.Size || s.Phase == "verified" && (!s.DestinationComplete || s.VerifiedUnixMS == 0) {
		return ErrInvalid
	}
	return nil
}
func private(i os.FileInfo, dir bool) error {
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Geteuid() || i.Mode().Perm()&0077 != 0 || i.IsDir() != dir || !dir && (!i.Mode().IsRegular() || s.Nlink != 1) {
		return ErrUnavailable
	}
	return nil
}
func open(path string, create bool) (*Journal, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrInvalid
	}
	if create {
		if e := os.MkdirAll(path, 0700); e != nil {
			return nil, e
		}
	}
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if e = private(info, true); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(path)
	if e != nil {
		return nil, e
	}
	j := &Journal{root: root, path: path, info: info}
	j.lock, e = root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e == nil {
		var i os.FileInfo
		i, e = j.lock.Stat()
		if e == nil {
			e = private(i, false)
		}
	}
	if e == nil {
		e = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if e != nil {
		j.Close()
		return nil, ErrUnavailable
	}
	if e = j.Check(); e != nil {
		j.Close()
		return nil, e
	}
	return j, nil
}
func (j *Journal) Check() error {
	if j.closed || j.fault {
		return ErrUnavailable
	}
	a, e := j.root.Stat(".")
	if e != nil || private(a, true) != nil || !os.SameFile(j.info, a) {
		return ErrUnavailable
	}
	b, e := os.Lstat(j.path)
	if e != nil || private(b, true) != nil || !os.SameFile(a, b) {
		return ErrUnavailable
	}
	return nil
}
func (j *Journal) read() (State, error) {
	var s State
	i, e := j.root.Lstat("state.json")
	if e != nil {
		return s, e
	}
	if e = private(i, false); e != nil {
		return s, e
	}
	f, e := j.root.OpenFile("state.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return s, e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || private(actual, false) != nil || !os.SameFile(i, actual) {
		return s, ErrUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(f, journalLimit+1))
	if e != nil || len(b) > journalLimit {
		return s, ErrInvalid
	}
	var env envelope
	if authority.DecodeStrictLimit(b, &env, journalLimit) != nil || env.SHA256 != hash(env.State) || authority.DecodeStrictLimit(env.State, &s, journalLimit) != nil || s.validate() != nil {
		return s, ErrInvalid
	}
	return s, nil
}
func (j *Journal) cleanPending() error {
	i, e := j.root.Lstat(".state.pending")
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil || private(i, false) != nil {
		return ErrUnavailable
	}
	return j.root.Remove(".state.pending")
}
func Create(path string, p Plan) (*Journal, error) {
	now := time.Now().UnixMilli()
	if p.Validate() != nil || now < p.PreparedUnixMS || now >= p.RetainUntilUnixMS {
		return nil, ErrInvalid
	}
	j, e := open(path, true)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			j.Close()
		}
	}()
	if _, e = j.root.Lstat("state.json"); !errors.Is(e, os.ErrNotExist) {
		return nil, errors.New("mirror plan already exists or cannot be checked")
	}
	if e = j.cleanPending(); e != nil {
		return nil, e
	}
	d, e := j.root.Open(".")
	if e != nil {
		return nil, e
	}
	names, e := d.Readdirnames(3)
	d.Close()
	if e != nil && !errors.Is(e, io.EOF) || len(names) != 1 || names[0] != ".lock" {
		return nil, errors.New("new mirror plan needs an empty private directory")
	}
	state := State{Plan: p, PlanSHA256: p.Digest(), Phase: "prepared", LastUnixMS: p.PreparedUnixMS}
	if e = j.save(state); e != nil {
		return nil, e
	}
	ok = true
	return j, nil
}
func Open(path string) (*Journal, error) {
	j, e := open(path, false)
	if e != nil {
		return nil, e
	}
	s, e := j.read()
	if e == nil {
		e = j.cleanPending()
	}
	if e != nil {
		j.Close()
		return nil, e
	}
	j.state = s
	return j, nil
}
func (j *Journal) Snapshot() State { return j.state }
func (j *Journal) save(s State) error {
	if e := j.Check(); e != nil {
		return e
	}
	if s.validate() != nil {
		return ErrInvalid
	}
	if e := j.cleanPending(); e != nil {
		return e
	}
	b, _ := json.Marshal(s)
	raw, _ := json.Marshal(envelope{hash(b), b})
	if len(raw) > journalLimit {
		return ErrInvalid
	}
	f, e := j.root.OpenFile(".state.pending", os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		j.fault = true
		return ErrUnavailable
	}
	if writeHook != nil {
		writeHook("synced-temp")
	}
	if e = j.Check(); e != nil {
		return e
	}
	if e = j.root.Rename(".state.pending", "state.json"); e == nil {
		if writeHook != nil {
			writeHook("renamed")
		}
		var d *os.File
		d, e = j.root.Open(".")
		if e == nil {
			e = d.Sync()
			d.Close()
		}
	}
	if e != nil {
		j.fault = true
		return ErrUnavailable
	}
	j.state = s
	return nil
}

// Update cannot change the immutable plan, lower progress or backdate a check.
func (j *Journal) Update(s State, now time.Time) error {
	if s.PlanSHA256 != j.state.PlanSHA256 || s.Plan.Digest() != j.state.PlanSHA256 || now.UnixMilli() < j.state.LastUnixMS || s.DestinationReceived < j.state.DestinationReceived || s.Attempts < j.state.Attempts || s.VerifiedUnixMS < j.state.VerifiedUnixMS {
		return ErrChanged
	}
	s.LastUnixMS = now.UnixMilli()
	if s.LastUnixMS >= s.Plan.RetainUntilUnixMS {
		s.Phase = "expired"
		s.LastError = "expired"
	}
	return j.save(s)
}
func (j *Journal) Status(now time.Time) (Status, error) {
	s := j.state
	if e := j.Update(s, now); e != nil {
		return Status{}, e
	}
	s = j.state
	v := Status{State: s, Availability: "not_verified"}
	if s.VerifiedUnixMS > 0 {
		v.Availability = "verified_at_last_check_current_availability_unknown"
	}
	if s.Phase == "expired" {
		v.Availability = "expired"
	}
	return v, nil
}
func (j *Journal) CacheDirectory() string { return filepath.Join(j.path, "cache") }
func (j *Journal) Close() error {
	if j.closed {
		return nil
	}
	j.closed = true
	if j.lock != nil {
		_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
		j.lock.Close()
	}
	if j.root != nil {
		return j.root.Close()
	}
	return nil
}
