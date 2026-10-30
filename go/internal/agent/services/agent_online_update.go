package services

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const agentReleaseOrigin = "https://storage.googleapis.com/wendyos-images-public"
const maxAgentArchiveSize int64 = 256 << 20

type onlineRelease struct {
	version, path, checksum string
	size                    int64
}
type onlineUpdater struct {
	installed, os, arch string
	mu                  sync.Mutex
	state               *pb.OnlineUpdateStatus
	client              *http.Client
	origin              string
}

func newOnlineUpdater() *onlineUpdater {
	return &onlineUpdater{installed: version.Version, os: runtime.GOOS, arch: runtime.GOARCH, origin: agentReleaseOrigin, client: &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || req.URL.Scheme != "https" || req.URL.Host != "storage.googleapis.com" {
			return errors.New("update download redirected outside the release server")
		}
		return nil
	}}}
}
func (u *onlineUpdater) snapshot() *pb.OnlineUpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state == nil {
		return &pb.OnlineUpdateStatus{InstalledVersion: u.installed, Phase: "idle"}
	}
	return proto.Clone(u.state).(*pb.OnlineUpdateStatus)
}
func (u *onlineUpdater) set(value *pb.OnlineUpdateStatus) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.state = proto.Clone(value).(*pb.OnlineUpdateStatus)
}
func onlineBusy(phase string) bool {
	return phase == "downloading" || phase == "installing" || phase == "restarting"
}
func (u *onlineUpdater) get(ctx context.Context, location string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release server returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("release response exceeds size limit")
	}
	return raw, nil
}
func (u *onlineUpdater) release(ctx context.Context) (onlineRelease, error) {
	if u.os != "linux" {
		return onlineRelease{}, status.Error(codes.Unimplemented, "direct online updates are supported by the Linux agent")
	}
	raw, err := u.get(ctx, u.origin+"/agent/manifest.json", 4<<20)
	if err != nil {
		return onlineRelease{}, err
	}
	var manifest struct {
		Latest   string `json:"latest"`
		Versions map[string]struct {
			Artifacts map[string]struct {
				Path, Checksum string
				Size           int64 `json:"size_bytes"`
			} `json:"artifacts"`
		} `json:"versions"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return onlineRelease{}, err
	}
	artifact, ok := manifest.Versions[manifest.Latest].Artifacts[u.arch]
	checksum, hashErr := hex.DecodeString(artifact.Checksum)
	relative, parseErr := url.Parse(artifact.Path)
	if manifest.Latest == "" || !ok || hashErr != nil || len(checksum) != sha256.Size || artifact.Size <= 0 || artifact.Size > maxAgentArchiveSize || parseErr != nil || relative.IsAbs() || relative.Host != "" || relative.RawQuery != "" || relative.Fragment != "" || strings.HasPrefix(artifact.Path, "/") || path.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "../") || !strings.HasPrefix(artifact.Path, "agent/") {
		return onlineRelease{}, errors.New("release manifest has no valid artifact for this device")
	}
	return onlineRelease{manifest.Latest, artifact.Path, strings.ToLower(artifact.Checksum), artifact.Size}, nil
}
func (s *AgentUpdateService) CheckOnlineUpdate(ctx context.Context, _ *pb.CheckOnlineUpdateRequest) (*pb.OnlineUpdateStatus, error) {
	current := s.online.snapshot()
	if onlineBusy(current.Phase) {
		return current, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rel, err := s.online.release(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "could not check for updates: %v", err)
	}
	result := &pb.OnlineUpdateStatus{InstalledVersion: s.online.installed, LatestVersion: rel.version, UpdateAvailable: version.CompareVersions(rel.version, s.online.installed) > 0, Phase: "current"}
	if result.UpdateAvailable {
		result.Phase = "available"
	}
	// A concurrent update may have started while the metadata request ran.
	s.online.mu.Lock()
	defer s.online.mu.Unlock()
	if s.online.state != nil && onlineBusy(s.online.state.Phase) {
		return proto.Clone(s.online.state).(*pb.OnlineUpdateStatus), nil
	}
	s.online.state = proto.Clone(result).(*pb.OnlineUpdateStatus)
	return result, nil
}
func (s *AgentUpdateService) GetOnlineUpdateStatus(context.Context, *pb.GetOnlineUpdateStatusRequest) (*pb.OnlineUpdateStatus, error) {
	return s.online.snapshot(), nil
}
func (s *AgentUpdateService) StartOnlineUpdate(ctx context.Context, req *pb.StartOnlineUpdateRequest) (*pb.OnlineUpdateStatus, error) {
	if req.GetVersion() == "" {
		return nil, status.Error(codes.InvalidArgument, "check for updates first")
	}
	if !s.installer.TryLock() {
		return nil, status.Error(codes.FailedPrecondition, "an update is already in progress")
	}
	rel, err := s.online.release(ctx)
	if err != nil {
		s.installer.Unlock()
		return nil, status.Errorf(codes.Unavailable, "could not check the release: %v", err)
	}
	if rel.version != req.Version || version.CompareVersions(rel.version, s.online.installed) <= 0 {
		s.installer.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "the available release changed; check for updates again")
	}
	result := &pb.OnlineUpdateStatus{InstalledVersion: s.online.installed, LatestVersion: rel.version, UpdateAvailable: true, Phase: "downloading", TotalBytes: rel.size}
	s.online.set(result)
	// Download belongs to the device and survives the phone closing its view.
	go s.installOnlineRelease(rel)
	return result, nil
}
func (s *AgentUpdateService) installOnlineRelease(rel onlineRelease) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	committed := false
	defer func() {
		if !committed {
			s.installer.Unlock()
		}
	}()
	fail := func(err error) {
		state := s.online.snapshot()
		state.Phase = "failed"
		state.Detail = err.Error()
		s.online.set(state)
	}
	raw, err := s.online.get(ctx, s.online.origin+"/"+rel.path, maxAgentArchiveSize)
	if err != nil {
		fail(err)
		return
	}
	if int64(len(raw)) != rel.size {
		fail(errors.New("download size does not match release manifest"))
		return
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != rel.checksum {
		fail(errors.New("release checksum does not match"))
		return
	}
	binary, err := onlineAgentBinary(raw, s.online.arch)
	if err != nil {
		fail(err)
		return
	}
	binaryDigest := sha256.Sum256(binary)
	// Keep the same pinned-signature policy as streamed updates. Releases without
	// signatures cannot bypass it if signature enforcement is enabled.
	if err = s.verifier.Verify(binaryDigest[:], nil); err != nil {
		fail(fmt.Errorf("release signature verification: %w", err))
		return
	}
	state := s.online.snapshot()
	state.Phase = "installing"
	state.DownloadedBytes = int64(len(raw))
	s.online.set(state)
	execPath, perm, err := s.resolvedExecPath()
	if err != nil {
		fail(err)
		return
	}
	file, tmp, cleanup, err := createUpdateTempFile(execPath)
	if err != nil {
		fail(err)
		return
	}
	defer func() {
		if !committed {
			cleanup()
		}
	}()
	if _, err = file.Write(binary); err != nil {
		fail(err)
		return
	}
	if _, err = commitBinaryUpdate(file, tmp, execPath, hex.EncodeToString(binaryDigest[:]), perm, s.logger); err != nil {
		if !errors.Is(err, ErrDirFsync) {
			fail(err)
			return
		}
		s.logger.Warn("Online update directory fsync failed", zap.Error(err))
	}
	committed = true
	state.Phase = "restarting"
	s.online.set(state)
	s.restartFn()
}
func onlineAgentBinary(raw []byte, arch string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxAgentBinarySize+(16<<20)))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || path.Base(header.Name) != "wendy-agent" {
			continue
		}
		if header.Size <= 0 || header.Size > maxAgentBinarySize {
			return nil, errors.New("invalid agent binary size")
		}
		binary, err := io.ReadAll(io.LimitReader(tr, maxAgentBinarySize+1))
		if err != nil {
			return nil, err
		}
		executable, err := elf.NewFile(bytes.NewReader(binary))
		if err != nil {
			return nil, errors.New("release does not contain a Linux executable")
		}
		defer executable.Close()
		expected := map[string]elf.Machine{"arm64": elf.EM_AARCH64, "amd64": elf.EM_X86_64}[arch]
		if expected == elf.EM_NONE || executable.Machine != expected {
			return nil, errors.New("release architecture does not match this device")
		}
		return binary, nil
	}
	return nil, errors.New("release does not contain wendy-agent")
}
