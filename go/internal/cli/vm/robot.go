package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

const (
	RobotProfileVersion   = 1
	RobotKindGo2          = "go2"
	RobotKindG1           = "g1"
	RobotSandboxGuestPort = 8890
	maxRobotProfileBytes  = 64 << 10
)

var (
	ErrRobotProfileExists  = errors.New("VM already has a robot profile")
	ErrRobotProfileMissing = errors.New("VM has no robot profile")
	robotDigestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	robotBundlePattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

// RobotProfile describes the desired robot independently of QEMU's run state.
// RuntimeDigest is empty until an image has been built; SourceDigest identifies
// the immutable source payload used to build it. Health and readiness are live
// observations and must never be inferred from this durable record.
type RobotProfile struct {
	Version         int    `json:"version"`
	Kind            string `json:"kind"`
	RuntimeDigest   string `json:"runtimeDigest,omitempty"`
	SourceDigest    string `json:"sourceDigest"`
	World           string `json:"world"`
	PolicyBundle    string `json:"policyBundle"`
	Interface       string `json:"interface"`
	DomainID        int    `json:"domainId"`
	ClockMode       string `json:"clockMode"`
	Seed            uint32 `json:"seed"`
	VisualDetail    string `json:"visualDetail"`
	CPUs            int    `json:"cpus"`
	MemoryMiB       int    `json:"memoryMiB"`
	SandboxHostPort int    `json:"sandboxHostPort,omitempty"`
}

func NewGo2RobotProfile(sourceDigest, policyBundle string) (RobotProfile, error) {
	return NewRobotProfile(RobotKindGo2, sourceDigest, policyBundle)
}

func NewG1RobotProfile(sourceDigest, policyBundle string) (RobotProfile, error) {
	return NewRobotProfile(RobotKindG1, sourceDigest, policyBundle)
}

func NewRobotProfile(kind, sourceDigest, policyBundle string) (RobotProfile, error) {
	p := RobotProfile{
		Version: RobotProfileVersion, Kind: kind,
		SourceDigest: sourceDigest, World: "indoor", PolicyBundle: policyBundle,
		Interface: "lo", DomainID: 0, ClockMode: "device", VisualDetail: "balanced",
		CPUs: DefaultCPUs, MemoryMiB: DefaultMemoryMiB,
	}
	return p, p.Validate()
}

// Validate is deliberately strict: an unsupported profile must not silently
// become a different robot or widen DDS discovery beyond the guest loopback.
func (p RobotProfile) Validate() error {
	switch {
	case p.Version != RobotProfileVersion:
		return fmt.Errorf("unsupported robot profile version %d", p.Version)
	case p.Kind != RobotKindGo2 && p.Kind != RobotKindG1:
		return fmt.Errorf("unsupported robot kind %q", p.Kind)
	case !robotDigestPattern.MatchString(p.SourceDigest):
		return fmt.Errorf("robot sourceDigest must be a lowercase sha256 digest")
	case p.RuntimeDigest != "" && !robotDigestPattern.MatchString(p.RuntimeDigest):
		return fmt.Errorf("robot runtimeDigest must be a lowercase sha256 digest")
	case p.World != "indoor":
		return fmt.Errorf("unsupported robot world %q", p.World)
	case !robotBundlePattern.MatchString(p.PolicyBundle):
		return fmt.Errorf("invalid robot policyBundle %q", p.PolicyBundle)
	case p.Interface != "lo":
		return fmt.Errorf("robot profile v1 requires interface lo")
	case p.DomainID != 0:
		return fmt.Errorf("robot profile v1 requires domainId 0")
	case p.ClockMode != "device":
		return fmt.Errorf("unsupported robot clockMode %q", p.ClockMode)
	case p.VisualDetail != "balanced" && p.VisualDetail != "full":
		return fmt.Errorf("unsupported robot visualDetail %q", p.VisualDetail)
	case p.CPUs < 1 || p.CPUs > 64:
		return fmt.Errorf("robot cpus must be between 1 and 64")
	case p.MemoryMiB < 256 || p.MemoryMiB > 1<<20:
		return fmt.Errorf("robot memoryMiB must be between 256 and 1048576")
	case p.SandboxHostPort < 0 || p.SandboxHostPort > 65535:
		return fmt.Errorf("invalid robot sandboxHostPort %d", p.SandboxHostPort)
	}
	return nil
}

func (s *Store) RobotProfilePath(name string) string {
	return filepath.Join(s.Dir(name), "robot.json")
}

// ReadRobotProfile distinguishes an ordinary VM (exists=false, err=nil) from
// an unreadable or unsupported robot record. Callers must surface the latter.
func (s *Store) ReadRobotProfile(name string) (profile RobotProfile, exists bool, err error) {
	if err := ValidName(name); err != nil {
		return profile, false, err
	}
	path := s.RobotProfilePath(name)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return profile, false, nil
	}
	if err != nil {
		return profile, false, err
	}
	if !info.Mode().IsRegular() {
		return profile, true, fmt.Errorf("robot profile is not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return profile, true, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRobotProfileBytes+1))
	if err != nil {
		return profile, true, err
	}
	if len(data) > maxRobotProfileBytes {
		return profile, true, fmt.Errorf("robot profile exceeds %d bytes", maxRobotProfileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&profile); err != nil {
		return profile, true, fmt.Errorf("reading robot profile: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return profile, true, fmt.Errorf("robot profile must contain exactly one JSON object")
	}
	if err := profile.Validate(); err != nil {
		return profile, true, err
	}
	return profile, true, nil
}

// CreateRobotProfile attaches a profile to an existing VM. Existing records,
// including unreadable ones, are never replaced by a creation attempt.
func (s *Store) CreateRobotProfile(name string, profile RobotProfile) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	lock, err := s.acquireLifecycleLock(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, ok := s.ReadMeta(name); !ok {
		return fmt.Errorf("VM %q has no readable metadata", name)
	}
	if _, exists, err := s.ReadRobotProfile(name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %s", ErrRobotProfileExists, name)
	}
	return writeJSON(s.RobotProfilePath(name), profile)
}

// UpdateRobotProfile serializes the complete read/modify/validate/replace
// operation. The callback receives the current record, never a stale snapshot.
func (s *Store) UpdateRobotProfile(name string, update func(*RobotProfile) error) error {
	if update == nil {
		return fmt.Errorf("robot profile update is nil")
	}
	lock, err := s.acquireLifecycleLock(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	profile, err := s.requireRobotProfile(name)
	if err != nil {
		return err
	}
	if err := update(&profile); err != nil {
		return err
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	return writeJSON(s.RobotProfilePath(name), profile)
}

func (s *Store) requireRobotProfile(name string) (RobotProfile, error) {
	profile, exists, err := s.ReadRobotProfile(name)
	if err != nil {
		return profile, err
	}
	if !exists {
		return profile, fmt.Errorf("%w: %s", ErrRobotProfileMissing, name)
	}
	return profile, nil
}

// EnsureRobotSandboxPort reconciles the guest's fixed HTTP port with its own
// QMP monitor, then persists only the verified host-side forward. A stale port
// owned by another VM is never treated as this robot's endpoint.
func (s *Store) EnsureRobotSandboxPort(ctx context.Context, name string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	lock, err := s.acquireLifecycleLock(name)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	profile, err := s.requireRobotProfile(name)
	if err != nil {
		return 0, err
	}
	preferred := profile.SandboxHostPort
	if preferred == 0 {
		preferred = RobotSandboxGuestPort
	}
	port, err := s.ensureTCPPortMapping(ctx, name, RobotSandboxGuestPort, preferred)
	if err != nil {
		return 0, err
	}
	if profile.SandboxHostPort != port {
		profile.SandboxHostPort = port
		if err := writeJSON(s.RobotProfilePath(name), profile); err != nil {
			return 0, fmt.Errorf("recording robot sandbox port: %w", err)
		}
	}
	return port, nil
}
