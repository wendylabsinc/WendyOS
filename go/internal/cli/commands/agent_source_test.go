package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const sampleAgentManifest = `{
  "latest": "2026.07.01-120000",
  "latest_nightly": "2026.07.03-093000",
  "versions": {
    "2026.07.03-093000": {
      "is_nightly": true,
      "artifacts": {
        "amd64": {"path": "agent/2026.07.03-093000/wendy-agent-linux-amd64-2026.07.03-093000.tar.gz", "checksum": "abc123", "size_bytes": 42},
        "arm64": {"path": "agent/2026.07.03-093000/wendy-agent-linux-arm64-2026.07.03-093000.tar.gz", "checksum": "def456", "size_bytes": 43}
      }
    },
    "2026.07.01-120000": {
      "is_nightly": false,
      "artifacts": {
        "arm64": {"path": "agent/2026.07.01-120000/wendy-agent-linux-arm64-2026.07.01-120000.tar.gz", "checksum": "aaa", "size_bytes": 10}
      }
    }
  }
}`

func TestFetchAgentManifestDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/manifest.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(sampleAgentManifest))
	}))
	defer srv.Close()

	m, err := fetchAgentManifestFrom(srv.URL)
	if err != nil {
		t.Fatalf("fetchAgentManifestFrom: %v", err)
	}
	if m.Latest != "2026.07.01-120000" || m.LatestNightly != "2026.07.03-093000" {
		t.Fatalf("pointers: latest=%q nightly=%q", m.Latest, m.LatestNightly)
	}
	v, ok := m.Versions["2026.07.03-093000"]
	if !ok || !v.IsNightly {
		t.Fatalf("nightly version entry missing or not flagged nightly: %+v", v)
	}
	if v.Artifacts["amd64"].Path == "" || v.Artifacts["amd64"].Checksum != "abc123" || v.Artifacts["amd64"].SizeBytes != 42 {
		t.Fatalf("amd64 artifact wrong: %+v", v.Artifacts["amd64"])
	}
}

func TestFetchAgentManifest404IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := fetchAgentManifestFrom(srv.URL); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func makeAgentTarGz(t *testing.T, innerName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: innerName, Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractAgentFromTarGz(t *testing.T) {
	payload := []byte("ELF-ish-binary")
	tgz := makeAgentTarGz(t, "wendy-agent-linux-amd64/wendy-agent", payload)
	got, err := extractAgentFromTarGz(bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("extractAgentFromTarGz: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q", got)
	}
}

func TestExtractAgentFromTarGzMissing(t *testing.T) {
	tgz := makeAgentTarGz(t, "some-dir/not-the-agent", []byte("x"))
	if _, err := extractAgentFromTarGz(bytes.NewReader(tgz)); err == nil {
		t.Fatal("expected error when wendy-agent absent")
	}
}

func TestAgentVersionFromManifest(t *testing.T) {
	m := &agentManifest{Latest: "2026.07.01-120000", LatestNightly: "2026.07.03-093000"}

	v, err := agentVersionFromManifest(m, false)
	if err != nil || v != "2026.07.01-120000" {
		t.Fatalf("stable: v=%q err=%v", v, err)
	}
	v, err = agentVersionFromManifest(m, true)
	if err != nil || v != "2026.07.03-093000" {
		t.Fatalf("nightly: v=%q err=%v", v, err)
	}
}

func TestAgentVersionFromManifestEmptyIsError(t *testing.T) {
	if _, err := agentVersionFromManifest(&agentManifest{}, false); err == nil {
		t.Fatal("expected error when no stable version present")
	}
	if _, err := agentVersionFromManifest(&agentManifest{Latest: "x"}, true); err == nil {
		t.Fatal("expected error when no nightly version present")
	}
}

func TestDownloadAgentFromGCS(t *testing.T) {
	payload := []byte("agent-binary-bytes")
	tgz := makeAgentTarGz(t, "wendy-agent-linux-amd64/wendy-agent", payload)
	sum := sha256.Sum256(tgz)
	checksum := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/manifest.json":
			fmt.Fprintf(w, `{"latest":"v1","versions":{"v1":{"is_nightly":false,"artifacts":{"amd64":{"path":"agent/v1/a.tar.gz","checksum":%q,"size_bytes":%d}}}}}`, checksum, len(tgz))
		case "/agent/v1/a.tar.gz":
			w.Write(tgz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m, err := fetchAgentManifestFrom(srv.URL)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	bin, ver, err := downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "", "amd64", false)
	if err != nil {
		t.Fatalf("downloadAgentArtifactFromGCS: %v", err)
	}
	if ver != "v1" || !bytes.Equal(bin, payload) {
		t.Fatalf("ver=%q bin=%q", ver, bin)
	}
}

func TestDownloadAgentFromGCSMissingArch(t *testing.T) {
	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{"amd64": {Path: "p"}}},
	}}
	if _, _, err := downloadAgentArtifactFromGCS(context.Background(), "http://unused", m, "", "arm64", false); err == nil {
		t.Fatal("expected error for missing arch")
	}
}

func TestDownloadAgentFromGCSChecksumMismatch(t *testing.T) {
	tgz := makeAgentTarGz(t, "wendy-agent-linux-amd64/wendy-agent", []byte("x"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/a.tar.gz" {
			w.Write(tgz)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{"amd64": {Path: "agent/v1/a.tar.gz", Checksum: "deadbeef"}}},
	}}
	if _, _, err := downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "", "amd64", false); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

const sampleAgentManifestWithDarwin = `{
  "latest": "2026.07.01-120000",
  "latest_nightly": "2026.07.03-093000",
  "versions": {
    "2026.07.01-120000": {
      "is_nightly": false,
      "artifacts": {
        "amd64": {"path": "agent/2026.07.01-120000/wendy-agent-linux-amd64-2026.07.01-120000.tar.gz", "checksum": "aaa", "size_bytes": 10},
        "arm64": {"path": "agent/2026.07.01-120000/wendy-agent-linux-arm64-2026.07.01-120000.tar.gz", "checksum": "bbb", "size_bytes": 11},
        "darwin-arm64": {"path": "agent/2026.07.01-120000/wendy-agent-macos-arm64-2026.07.01-120000.zip", "checksum": "ccc", "size_bytes": 12}
      }
    }
  }
}`

func TestFetchAgentManifestDecodesDarwinArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/manifest.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(sampleAgentManifestWithDarwin))
	}))
	defer srv.Close()

	m, err := fetchAgentManifestFrom(srv.URL)
	if err != nil {
		t.Fatalf("fetchAgentManifestFrom: %v", err)
	}
	v, ok := m.Versions["2026.07.01-120000"]
	if !ok {
		t.Fatalf("version entry missing")
	}
	art, ok := v.Artifacts["darwin-arm64"]
	if !ok {
		t.Fatalf("darwin-arm64 artifact missing: %+v", v.Artifacts)
	}
	if art.Checksum != "ccc" || art.SizeBytes != 12 || art.Path == "" {
		t.Fatalf("darwin-arm64 artifact wrong: %+v", art)
	}
}

func TestManifestArtifactKey(t *testing.T) {
	tests := []struct {
		name   string
		osName string
		arch   string
		want   string
	}{
		{"empty os", "", "arm64", "arm64"},
		{"wendyos", "wendyos", "arm64", "arm64"},
		{"darwin lowercase", "darwin", "arm64", "darwin-arm64"},
		{"darwin mixed case", "Darwin", "arm64", "darwin-arm64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manifestArtifactKey(tt.osName, tt.arch); got != tt.want {
				t.Fatalf("manifestArtifactKey(%q, %q) = %q, want %q", tt.osName, tt.arch, got, tt.want)
			}
		})
	}
}

func TestAgentPlatformLabel(t *testing.T) {
	tests := []struct {
		name   string
		osName string
		arch   string
		want   string
	}{
		{"darwin", "darwin", "arm64", "macos/arm64"},
		{"darwin mixed case", "Darwin", "arm64", "macos/arm64"},
		{"ubuntu", "ubuntu", "arm64", "linux/arm64"},
		{"empty os", "", "arm64", "linux/arm64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentPlatformLabel(tt.osName, tt.arch); got != tt.want {
				t.Fatalf("agentPlatformLabel(%q, %q) = %q, want %q", tt.osName, tt.arch, got, tt.want)
			}
		})
	}
}

func sampleReleaseAssets() []githubReleaseAsset {
	return []githubReleaseAsset{
		{Name: "wendy-agent-linux-amd64-v1.tar.gz"},
		{Name: "wendy-agent-linux-arm64-v1.tar.gz"},
		{Name: "wendy-agent-macos-arm64-v1.zip"},
		{Name: "wendy-agent-linux-arm64-v1.tar.gz.sha256"},
		{Name: "checksums.txt"},
	}
}

func TestMatchAgentReleaseAsset(t *testing.T) {
	assets := sampleReleaseAssets()

	t.Run("darwin arm64 picks the zip", func(t *testing.T) {
		a, err := matchAgentReleaseAsset(assets, "darwin", "arm64", "v1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.Name != "wendy-agent-macos-arm64-v1.zip" {
			t.Fatalf("matched wrong asset: %q", a.Name)
		}
	})

	t.Run("linux arm64 picks the tarball, not the zip", func(t *testing.T) {
		a, err := matchAgentReleaseAsset(assets, "ubuntu", "arm64", "v1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a.Name != "wendy-agent-linux-arm64-v1.tar.gz" {
			t.Fatalf("matched wrong asset: %q", a.Name)
		}
	})

	t.Run("darwin amd64 missing errors with macos label and tag", func(t *testing.T) {
		_, err := matchAgentReleaseAsset(assets, "darwin", "amd64", "v1")
		if err == nil {
			t.Fatal("expected error for missing darwin/amd64 asset")
		}
		wantMsg := "no asset for macos/amd64 in release v1"
		if err.Error() != wantMsg {
			t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
		}
	})

	t.Run("linux missing keeps existing error wording", func(t *testing.T) {
		_, err := matchAgentReleaseAsset(assets, "", "riscv64", "v1")
		if err == nil {
			t.Fatal("expected error for missing linux/riscv64 asset")
		}
		wantMsg := "no asset for linux/riscv64 in release v1"
		if err.Error() != wantMsg {
			t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
		}
	})
}

func TestDownloadAgentArtifactFromGCSDarwinReturnsRawZipBytes(t *testing.T) {
	// A payload that is deliberately NOT a valid tar.gz — proves the darwin
	// path never attempts extraction and returns the bytes verbatim.
	zipPayload := []byte("PK\x03\x04-this-is-not-a-tarball-fake-zip-bytes")
	sum := sha256.Sum256(zipPayload)
	checksum := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/a.zip":
			w.Write(zipPayload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{
			"darwin-arm64": {Path: "agent/v1/a.zip", Checksum: checksum, SizeBytes: int64(len(zipPayload))},
		}},
	}}

	got, ver, err := downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "darwin", "arm64", false)
	if err != nil {
		t.Fatalf("downloadAgentArtifactFromGCS: %v", err)
	}
	if ver != "v1" {
		t.Fatalf("version = %q, want v1", ver)
	}
	if !bytes.Equal(got, zipPayload) {
		t.Fatalf("payload mismatch: got %q, want %q", got, zipPayload)
	}
}

func TestDownloadAgentArtifactFromGCSDarwinChecksumMismatch(t *testing.T) {
	zipPayload := []byte("PK\x03\x04-fake-zip-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/a.zip" {
			w.Write(zipPayload)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{
			"darwin-arm64": {Path: "agent/v1/a.zip", Checksum: "deadbeef"},
		}},
	}}
	if _, _, err := downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "darwin", "arm64", false); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

func TestDownloadAgentArtifactFromGCSMissingDarwinKey(t *testing.T) {
	// Only the bare-arch (linux) key is present; the darwin-<arch> key is
	// absent, which is what drives resolveAgentArtifact's GitHub fallback for
	// a darwin caller against an old manifest.
	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{"arm64": {Path: "agent/v1/a.tar.gz"}}},
	}}
	_, _, err := downloadAgentArtifactFromGCS(context.Background(), "http://unused", m, "darwin", "arm64", false)
	if err == nil {
		t.Fatal("expected error for missing darwin-arm64 key")
	}
	wantMsg := "agent manifest has no macos/arm64 artifact for version v1"
	if err.Error() != wantMsg {
		t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
	}
}

func TestDownloadAgentArtifactFromGCSMissingLinuxKeyKeepsExistingWording(t *testing.T) {
	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{"amd64": {Path: "p"}}},
	}}
	_, _, err := downloadAgentArtifactFromGCS(context.Background(), "http://unused", m, "", "arm64", false)
	if err == nil {
		t.Fatal("expected error for missing arch")
	}
	wantMsg := "agent manifest has no linux/arm64 artifact for version v1"
	if err.Error() != wantMsg {
		t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
	}
}

// TestDownloadAgentArtifactFromGCSErrorWordingIsPlatformAware pins the darwin
// vs non-darwin transfer-error noun: "agent artifact" on darwin (finding 3),
// "agent tarball" everywhere else, byte-identical to the pre-existing wording.
func TestDownloadAgentArtifactFromGCSErrorWordingIsPlatformAware(t *testing.T) {
	m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
		"v1": {Artifacts: map[string]agentManifestArtifact{
			"arm64":        {Path: "agent/v1/missing.tar.gz"},
			"darwin-arm64": {Path: "agent/v1/missing.zip"},
		}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "", "arm64", false)
	if err == nil {
		t.Fatal("expected error for 404 linux download")
	}
	if wantMsg := "agent tarball returned status 404"; err.Error() != wantMsg {
		t.Fatalf("linux wording changed: got %q, want %q", err.Error(), wantMsg)
	}

	_, _, err = downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "darwin", "arm64", false)
	if err == nil {
		t.Fatal("expected error for 404 darwin download")
	}
	if wantMsg := "agent artifact returned status 404"; err.Error() != wantMsg {
		t.Fatalf("darwin wording wrong: got %q, want %q", err.Error(), wantMsg)
	}
}

// TestDownloadAgentArtifactFromGCSDarwinZipSanityCheck covers finding 2: the
// GCS darwin payload must look like a zip (magic "PK\x03\x04") before it's
// trusted as the app-bundle artifact, while a well-formed zip payload still
// round-trips unchanged.
func TestDownloadAgentArtifactFromGCSDarwinZipSanityCheck(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		wantErr bool
	}{
		{"valid zip magic bytes are accepted", []byte("PK\x03\x04-fake-but-zip-shaped-payload"), false},
		{"non-zip payload is rejected", []byte("this-is-definitely-not-a-zip-file"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := sha256.Sum256(tt.payload)
			checksum := hex.EncodeToString(sum[:])

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/agent/v1/a.zip" {
					w.Write(tt.payload)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			m := &agentManifest{Latest: "v1", Versions: map[string]agentManifestVersion{
				"v1": {Artifacts: map[string]agentManifestArtifact{
					"darwin-arm64": {Path: "agent/v1/a.zip", Checksum: checksum, SizeBytes: int64(len(tt.payload))},
				}},
			}}

			bin, ver, err := downloadAgentArtifactFromGCS(context.Background(), srv.URL, m, "darwin", "arm64", false)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				wantMsg := "downloaded macOS agent artifact is not a zip archive"
				if err.Error() != wantMsg {
					t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ver != "v1" || !bytes.Equal(bin, tt.payload) {
				t.Fatalf("ver=%q bin=%q", ver, bin)
			}
		})
	}
}

// TestCheckDarwinArtifactVersion covers finding 1's version-skew guard:
// match, mismatch, and non-darwin passthrough (the check must never fire for
// linux/other platforms, even on a genuine version mismatch).
func TestCheckDarwinArtifactVersion(t *testing.T) {
	tests := []struct {
		name    string
		osName  string
		target  string
		actual  string
		wantErr bool
	}{
		{"darwin match is ok", "darwin", "v2", "v2", false},
		{"darwin mismatch errors", "darwin", "v2", "v1", true},
		{"darwin mixed-case os still matches", "Darwin", "v2", "v2", false},
		{"darwin mixed-case os still errors on mismatch", "Darwin", "v2", "v1", true},
		{"darwin empty target is a no-op", "darwin", "", "v1", false},
		{"darwin empty actual is a no-op", "darwin", "v2", "", false},
		{"non-darwin mismatch is a passthrough", "ubuntu", "v2", "v1", false},
		{"empty os mismatch is a passthrough", "", "v2", "v1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDarwinArtifactVersion(tt.osName, tt.target, tt.actual)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckDarwinArtifactVersionMismatchMessage(t *testing.T) {
	err := checkDarwinArtifactVersion("darwin", "v2", "v1")
	if err == nil {
		t.Fatal("expected error")
	}
	wantMsg := "macOS agent artifact for v2 is not published yet (latest available: v1); try again once the release completes"
	if err.Error() != wantMsg {
		t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
	}
}
