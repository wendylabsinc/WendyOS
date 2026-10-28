package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// agentManifest indexes wendy-agent versions published to GCS. It mirrors the
// master.json conventions in manifest.go; Latest points at the newest stable
// version and LatestNightly at the newest prerelease.
type agentManifest struct {
	Latest        string                          `json:"latest"`
	LatestNightly string                          `json:"latest_nightly"`
	Versions      map[string]agentManifestVersion `json:"versions"`
}

// agentManifestVersion.Artifacts is keyed by manifestArtifactKey(osName, arch):
// bare GOARCH ("amd64", "arm64") for the legacy Linux tarballs, and
// "darwin-<arch>" (e.g. "darwin-arm64") for the macOS app-bundle zip.
type agentManifestVersion struct {
	IsNightly bool                             `json:"is_nightly"`
	Artifacts map[string]agentManifestArtifact `json:"artifacts"`
}

type agentManifestArtifact struct {
	Path      string `json:"path"`     // bucket-relative, joined as gcsBaseURL + "/" + Path
	Checksum  string `json:"checksum"` // sha256 hex of the artifact (.tar.gz on linux, .zip on darwin)
	SizeBytes int64  `json:"size_bytes"`
}

// manifestArtifactKey returns the key under agentManifestVersion.Artifacts for
// osName/arch: the bare arch for every platform except darwin, which is keyed
// "darwin-<arch>" to distinguish the macOS app-bundle zip from the legacy
// Linux tarballs.
func manifestArtifactKey(osName, arch string) string {
	if strings.EqualFold(osName, "darwin") {
		return "darwin-" + arch
	}
	return arch
}

// agentPlatformLabel renders osName/arch for user-facing messages and error
// wording: "macos/<arch>" for darwin, "linux/<arch>" otherwise.
func agentPlatformLabel(osName, arch string) string {
	if strings.EqualFold(osName, "darwin") {
		return "macos/" + arch
	}
	return "linux/" + arch
}

func fetchAgentManifestFrom(baseURL string) (*agentManifest, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(baseURL + "/agent/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("fetching agent manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent manifest returned status %d", resp.StatusCode)
	}
	var m agentManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding agent manifest: %w", err)
	}
	return &m, nil
}

func fetchAgentManifest() (*agentManifest, error) {
	return fetchAgentManifestFrom(gcsBaseURL)
}

// extractAgentFromTarGz reads a gzipped tar stream and returns the bytes of the
// file whose name ends in "wendy-agent".
func extractAgentFromTarGz(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("opening gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && strings.HasSuffix(hdr.Name, "wendy-agent") {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading binary from tar: %w", err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("wendy-agent binary not found in tarball")
}

func agentVersionFromManifest(m *agentManifest, nightly bool) (string, error) {
	v := m.Latest
	if nightly {
		v = m.LatestNightly
	}
	if v == "" {
		if nightly {
			return "", fmt.Errorf("agent manifest has no latest_nightly version")
		}
		return "", fmt.Errorf("agent manifest has no latest version")
	}
	return v, nil
}

// resolveAgentVersion returns the latest wendy-agent version tag for the channel,
// preferring the GCS manifest and falling back to GitHub releases on any GCS miss.
func resolveAgentVersion(nightly bool) (version, source string, err error) {
	if m, mErr := fetchAgentManifest(); mErr == nil {
		if v, vErr := agentVersionFromManifest(m, nightly); vErr == nil {
			return v, "gcs", nil
		}
	}

	rel, err := fetchAgentRelease(nightly)
	if err != nil {
		return "", "", err
	}
	return rel.TagName, "github", nil
}

// downloadAgentArtifactFromGCS downloads and verifies the agent artifact for
// osName/arch from the manifest, returning the raw payload bytes. For darwin
// the payload is the app-bundle zip, returned verbatim (the Swift agent
// extracts/verifies it); for every other OS it's the extracted ELF binary
// pulled out of the .tar.gz.
func downloadAgentArtifactFromGCS(baseURL string, m *agentManifest, osName, arch string, nightly bool) ([]byte, string, error) {
	version, err := agentVersionFromManifest(m, nightly)
	if err != nil {
		return nil, "", err
	}
	ver, ok := m.Versions[version]
	if !ok {
		return nil, "", fmt.Errorf("agent manifest missing version entry %q", version)
	}
	art, ok := ver.Artifacts[manifestArtifactKey(osName, arch)]
	if !ok || art.Path == "" {
		return nil, "", fmt.Errorf("agent manifest has no %s artifact for version %s", agentPlatformLabel(osName, arch), version)
	}

	// The GCS payload is a .tar.gz on every platform except darwin, where it's
	// the raw app-bundle .zip — keep the error wording accurate to what's
	// actually being fetched without touching the non-darwin (tarball) text
	// pinned by existing tests.
	artifactNoun := "agent tarball"
	if strings.EqualFold(osName, "darwin") {
		artifactNoun = "agent artifact"
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(baseURL + "/" + art.Path)
	if err != nil {
		return nil, "", fmt.Errorf("downloading %s: %w", artifactNoun, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s returned status %d", artifactNoun, resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading %s: %w", artifactNoun, err)
	}
	if art.Checksum != "" {
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != art.Checksum {
			return nil, "", fmt.Errorf("%s checksum mismatch: manifest %s, got %s", artifactNoun, art.Checksum, got)
		}
	}
	if strings.EqualFold(osName, "darwin") {
		if !isZipArchive(raw) {
			return nil, "", fmt.Errorf("downloaded macOS agent artifact is not a zip archive")
		}
		return raw, version, nil
	}
	bin, err := extractAgentFromTarGz(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	return bin, version, nil
}

// resolveAgentArtifact returns the wendy-agent release artifact for
// osName/arch, preferring GCS (to avoid GitHub rate limits) and falling back
// to GitHub releases on any GCS miss. On darwin the payload is the raw
// app-bundle zip; on every other OS it's the extracted ELF binary. version is
// the resolved version tag; source is "gcs" or "github".
func resolveAgentArtifact(osName, arch string, nightly bool) (payload []byte, version, source string, err error) {
	if m, mErr := fetchAgentManifest(); mErr == nil {
		if bin, ver, dErr := downloadAgentArtifactFromGCS(gcsBaseURL, m, osName, arch, nightly); dErr == nil {
			return bin, ver, "gcs", nil
		}
	}

	rel, err := fetchAgentRelease(nightly)
	if err != nil {
		return nil, "", "", err
	}
	asset, err := matchAgentReleaseAsset(rel.Assets, osName, arch, rel.TagName)
	if err != nil {
		return nil, "", "", err
	}
	raw, err := downloadReleaseAssetBytes(*asset)
	if err != nil {
		return nil, "", "", err
	}
	if strings.EqualFold(osName, "darwin") {
		if !isZipArchive(raw) {
			return nil, "", "", fmt.Errorf("downloaded macOS agent artifact is not a zip archive")
		}
		return raw, rel.TagName, "github", nil
	}
	bin, err := extractAgentFromTarGz(bytes.NewReader(raw))
	if err != nil {
		return nil, "", "", err
	}
	return bin, rel.TagName, "github", nil
}

// Seams over the agent release lookups for tests.
var (
	resolveAgentVersionFn  = resolveAgentVersion
	resolveAgentArtifactFn = resolveAgentArtifact
)

// checkDarwinArtifactVersion guards against a macOS-specific version-skew
// window: the mac build runs in a separate, slower job from the GCS manifest
// publish, so "latest" can advance to a release whose macOS zip doesn't exist
// anywhere yet. In that window resolveAgentArtifact's GitHub fallback finds
// the newest release that DOES have a mac asset — which can be the version
// the Mac already runs — so uploading it would be a silent no-op update that
// still prints success and, worse, loops forever on every future check.
// Callers that independently resolved a target release version should pass
// it here alongside the version of the artifact resolveAgentArtifact actually
// returned; a mismatch fails loudly instead of uploading. target/actual is
// empty or equal in the common case (no skew), where this is a no-op. Only
// darwin is checked: non-darwin platforms publish tarball and manifest
// together, so there is no equivalent skew window to guard against.
func checkDarwinArtifactVersion(osName, target, actual string) error {
	if !strings.EqualFold(osName, "darwin") {
		return nil
	}
	if target == "" || actual == "" || target == actual {
		return nil
	}
	return fmt.Errorf("macOS agent artifact for %s is not published yet (latest available: %s); try again once the release completes", target, actual)
}

// matchAgentReleaseAsset finds the release asset for osName/arch: darwin
// matches "wendy-agent-macos-<arch>-" + ".zip", everything else matches the
// legacy "wendy-agent-linux-<arch>-" + ".tar.gz" prefix/suffix.
func matchAgentReleaseAsset(assets []githubReleaseAsset, osName, arch, tagName string) (*githubReleaseAsset, error) {
	var assetPrefix, assetSuffix string
	if strings.EqualFold(osName, "darwin") {
		assetPrefix = fmt.Sprintf("wendy-agent-macos-%s-", arch)
		assetSuffix = ".zip"
	} else {
		assetPrefix = fmt.Sprintf("wendy-agent-linux-%s-", arch)
		assetSuffix = ".tar.gz"
	}
	for i := range assets {
		a := assets[i]
		if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, assetSuffix) {
			return &a, nil
		}
	}
	return nil, fmt.Errorf("no asset for %s in release %s", agentPlatformLabel(osName, arch), tagName)
}
